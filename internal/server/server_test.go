package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/openai"
	"agyrelay/internal/pool"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const testKey = "sk-test"

type staticKeys map[string]conv.Caller

func (k staticKeys) Lookup(token string) (conv.Caller, bool) {
	caller, ok := k[token]
	return caller, ok
}

type relay struct {
	t   *testing.T
	url string
	log string
}

func newRelay(t *testing.T) *relay {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "acp.jsonl")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := bridge.Start(logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Config{
		Bin:             os.Args[0],
		Accounts:        []pool.AccountConfig{{Name: "main", Home: filepath.Join(dir, "home"), Env: []string{agytest.EnvRun + "=1", "FAKE_AGY_LOG=" + logPath}}},
		TurnsPerAccount: 4,
		Log:             logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Keys: staticKeys{testKey: {Name: "alice"}}, OpenAI: openai.NewHandler(gw)}))
	t.Cleanup(func() {
		srv.Close()
		gw.Close()
		p.Close()
		b.Close()
	})
	return &relay{t: t, url: srv.URL, log: logPath}
}

func (r *relay) post(path, key string, body any) (int, []byte) {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.url+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// prompts counts session/prompt requests agy received.
func (r *relay) prompts() int {
	r.t.Helper()
	raw, err := os.ReadFile(r.log)
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"method":"session/prompt"`) {
			n++
		}
	}
	return n
}

var readTool = map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}

func TestAuthRequired(t *testing.T) {
	r := newRelay(t)
	status, raw := r.post("/v1/responses", "wrong", map[string]any{"model": "gemini-3.8-flash", "input": "hi"})
	if status != http.StatusUnauthorized || !strings.Contains(string(raw), `"code":"invalid_api_key"`) {
		t.Fatalf("status %d body=%s", status, raw)
	}
}

func TestRootAbortsWithoutResponse(t *testing.T) {
	r := newRelay(t)
	resp, err := http.DefaultClient.Get(r.url + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("GET / returned status %d, want connection closed without HTTP response", resp.StatusCode)
	}
}

func TestBillingEndpoints(t *testing.T) {
	r := newRelay(t)
	for _, path := range []string{
		"/v1/dashboard/billing/subscription",
		"/dashboard/billing/subscription",
		"/v1/dashboard/billing/usage",
		"/dashboard/billing/usage",
	} {
		req, _ := http.NewRequest(http.MethodGet, r.url+path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
	}
}

// The client returns the output items verbatim plus its tool result; the
// relay must recognize its own transcript and continue the parked turn.
func TestResponsesToolRoundTripContinuesTurn(t *testing.T) {
	r := newRelay(t)
	input := []any{map[string]any{"role": "user", "content": `@tool read {"path":"a"}`}}
	status, raw := r.post("/v1/responses", testKey, map[string]any{"model": "gemini-3.8-flash", "input": input, "tools": []any{readTool}})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var first struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	callID := ""
	for _, item := range first.Output {
		input = append(input, item)
		if item["type"] == "function_call" {
			callID = item["call_id"].(string)
		}
	}
	if callID == "" {
		t.Fatalf("no function_call in %s", raw)
	}
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": "contents"})

	status, raw = r.post("/v1/responses", testKey, map[string]any{"model": "gemini-3.8-flash", "input": input, "tools": []any{readTool}})
	if status != http.StatusOK || !strings.Contains(string(raw), "tool said: contents") {
		t.Fatalf("status %d: %s", status, raw)
	}
	if n := r.prompts(); n != 1 {
		t.Fatalf("session/prompt count = %d, want 1 (continued turn)", n)
	}
}

// A Chat client that returns reasoning_details continues warm.
func TestChatReasoningDetailsRoundTrip(t *testing.T) {
	r := newRelay(t)
	messages := []any{map[string]any{"role": "user", "content": "hello"}}
	status, raw := r.post("/v1/chat/completions", testKey, map[string]any{"model": "gemini-3.8-flash", "messages": messages})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var first struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	message := first.Choices[0].Message
	if message["reasoning_content"] != "thinking" || len(message["reasoning_details"].([]any)) != 2 {
		t.Fatalf("message = %v", message)
	}
	messages = append(messages, message, map[string]any{"role": "user", "content": "again"})
	status, raw = r.post("/v1/chat/completions", testKey, map[string]any{"model": "gemini-3.8-flash", "messages": messages})
	if status != http.StatusOK || !strings.Contains(string(raw), "reply to: again") {
		t.Fatalf("status %d: %s", status, raw)
	}
	if n := r.prompts(); n != 2 {
		t.Fatalf("session/prompt count = %d, want 2", n)
	}
}

type exhaustedBudget struct{}

func (exhaustedBudget) Spent(context.Context, string) (map[string]float64, error) {
	return map[string]float64{"gemini": 1.0}, nil
}
func (exhaustedBudget) Priced(string) bool { return true }

func TestKeySpendLimitIsNonRetryable403(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := bridge.Start(logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Config{
		Bin:             os.Args[0],
		Accounts:        []pool.AccountConfig{{Name: "main", Home: filepath.Join(dir, "home"), Env: []string{agytest.EnvRun + "=1"}}},
		TurnsPerAccount: 4,
		Log:             logger,
	})
	if err != nil {
		b.Close()
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Budget: exhaustedBudget{}, Log: logger})
	if err != nil {
		p.Close()
		b.Close()
		t.Fatal(err)
	}
	limit := 1.0
	srv := httptest.NewServer(New(Config{
		Keys:   staticKeys{testKey: {Name: "alice", KeyHash: "h1", LimitTotal: &limit}},
		OpenAI: openai.NewHandler(gw),
	}))
	t.Cleanup(func() {
		srv.Close()
		gw.Close()
		p.Close()
		b.Close()
	})

	r := &relay{t: t, url: srv.URL}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		status, raw := r.post(path, testKey, map[string]any{
			"model":    "gemini-3.8-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"input":    "hi",
		})
		if status != http.StatusForbidden || !strings.Contains(string(raw), `"code":"insufficient_quota"`) {
			t.Fatalf("%s status=%d body=%s, want 403 insufficient_quota", path, status, raw)
		}
	}
}
