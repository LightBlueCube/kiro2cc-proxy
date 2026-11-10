package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type harness struct {
	t   *testing.T
	gw  *Gateway
	p   *pool.Pool
	log string
}

// newHarness runs a gateway on fake agy accounts; extraEnv adds
// environment per account index.
func newHarness(t *testing.T, accounts int, extraEnv map[int][]string) *harness {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "acp.jsonl")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	var configs []pool.AccountConfig
	for i := range accounts {
		env := append([]string{agytest.EnvRun + "=1", "FAKE_AGY_LOG=" + logPath}, extraEnv[i]...)
		configs = append(configs, pool.AccountConfig{Name: fmt.Sprintf("acct%d", i), Home: filepath.Join(dir, fmt.Sprintf("home%d", i)), Env: env})
	}
	b, err := bridge.Start(logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Config{Bin: os.Args[0], Accounts: configs, TurnsPerAccount: 4, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gw.Close()
		p.Close()
		b.Close()
	})
	return &harness{t: t, gw: gw, p: p, log: logPath}
}

type result struct {
	text      string
	reasoning string
	calls     []conv.ToolCall
	usage     *conv.Usage
	finish    conv.FinishReason
	err       *conv.Error
}

func (h *harness) complete(ctx context.Context, user string, req conv.Request) result {
	h.t.Helper()
	var r result
	for ev := range h.gw.Complete(ctx, user, req) {
		r.text += ev.Text
		r.reasoning += ev.Reasoning
		r.calls = append(r.calls, ev.ToolCalls...)
		if ev.Usage != nil {
			r.usage = ev.Usage
		}
		if ev.Finish != "" {
			r.finish = ev.Finish
		}
		if ev.Err != nil {
			r.err = ev.Err
		}
	}
	return r
}

type acpRequest struct {
	Method string `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Resource *struct {
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"prompt"`
	} `json:"params"`
}

// requests returns the logged ACP requests with the given method.
func (h *harness) requests(method string) []acpRequest {
	h.t.Helper()
	file, err := os.Open(h.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no agy has started yet
	}
	if err != nil {
		h.t.Fatal(err)
	}
	defer file.Close()
	var out []acpRequest
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var req acpRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			h.t.Fatalf("bad log line: %v", err)
		}
		if req.Method == method {
			out = append(out, req)
		}
	}
	return out
}

var readTool = conv.ToolSpec{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}

func user(text string) conv.Message { return conv.Message{Role: conv.RoleUser, Text: text} }

func TestPlainReplyThenWarmDelta(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{{Role: conv.RoleSystem, Text: "be brief"}, user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil || first.finish != conv.FinishStop || first.text != "reply to: hello" {
		t.Fatalf("first = %+v", first)
	}
	if first.usage == nil || first.usage.PromptTokens != 100 || first.usage.CompletionTokens != 15 {
		t.Fatalf("usage = %+v", first.usage)
	}

	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if second.err != nil || second.text != "reply to: again" {
		t.Fatalf("second = %+v", second)
	}
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new count = %d, want 1 (warm reuse)", n)
	}
	prompts := h.requests("session/prompt")
	if len(prompts) != 2 || prompts[1].Params.Prompt[0].Text != "again" {
		t.Fatalf("warm prompt = %+v", prompts)
	}

	// Another user never sees alice's session.
	other := h.complete(ctx, "bob", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if other.err != nil {
		t.Fatal(other.err)
	}
	if n := len(h.requests("session/new")); n != 2 {
		t.Fatalf("session/new count = %d, want 2", n)
	}
}

func TestToolRoundTripContinuesTurn(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	messages := []conv.Message{user(`@tool read {"path":"a.txt"}`)}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if first.err != nil || first.finish != conv.FinishToolCalls || len(first.calls) != 1 || first.calls[0].Name != "read" {
		t.Fatalf("first = %+v", first)
	}
	if string(first.calls[0].Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %s", first.calls[0].Arguments)
	}

	call := first.calls[0]
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Text: "contents of a"},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err != nil || second.finish != conv.FinishStop || second.text != "tool said: contents of a" {
		t.Fatalf("second = %+v", second)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("session/prompt count = %d, want 1 (same turn)", n)
	}
}

func TestParallelExpandsIntoToolCalls(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	messages := []conv.Message{user("@parallel read read")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if first.err != nil || len(first.calls) != 2 {
		t.Fatalf("first = %+v", first)
	}
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls})
	for i, call := range first.calls {
		messages = append(messages, conv.Message{Role: conv.RoleTool, ToolCallID: call.ID, Text: fmt.Sprintf("result %d", i)})
	}
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err != nil || !strings.Contains(second.text, "result 0") || !strings.Contains(second.text, "result 1") {
		t.Fatalf("second = %+v", second)
	}
}

func TestEditedToolResultsWarmDelta(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	messages := []conv.Message{user(`@tool read {}`)}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if first.err != nil {
		t.Fatal(first.err)
	}
	// The client answers and adds an assistant message of its own: the
	// parked turn cannot take that, so it is cancelled and the session
	// gets a transcript delta.
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, Text: "x"},
		conv.Message{Role: conv.RoleAssistant, Text: "other model"},
		user("continue"),
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err != nil || second.finish != conv.FinishStop {
		t.Fatalf("second = %+v", second)
	}
	prompts := h.requests("session/prompt")
	if len(prompts) != 2 || prompts[1].Params.Prompt[0].Resource == nil || !strings.Contains(prompts[1].Params.Prompt[0].Resource.Text, "other model") {
		t.Fatalf("delta prompt = %+v", prompts)
	}
	if prompts[0].Params.SessionID != prompts[1].Params.SessionID {
		t.Fatal("delta went to a different session")
	}
}

func TestQuotaNoticeRetriesOnAnotherAccount(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	// Find a user whose first-choice account is the exhausted one.
	name := ""
	for i := range 100 {
		candidate := fmt.Sprintf("user%d", i)
		account, err := h.p.Pick(candidate, "gemini", nil)
		if err != nil {
			t.Fatal(err)
		}
		account.Release()
		if account.Name() == "acct0" {
			name = candidate
			break
		}
	}
	if name == "" {
		t.Fatal("no user prefers acct0")
	}
	r := h.complete(context.Background(), name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err != nil || r.text != "reply to: hi" {
		t.Fatalf("result = %+v", r)
	}
	if err := h.p.Account("acct0").Available("gemini"); err == nil {
		t.Fatal("acct0 is not cooling down")
	}
}

func TestQuotaNoticeWithoutFallbackIs429(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrQuota || r.err.RetryAfterSeconds <= 0 || r.text != "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestDisconnectCancelsTurn(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result)
	go func() { done <- h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@hang")}}) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.requests("session/prompt")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("prompt never sent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	for len(h.requests("session/cancel")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session/cancel never sent")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUnknownModel(t *testing.T) {
	h := newHarness(t, 1, nil)
	r := h.complete(context.Background(), "alice", conv.Request{Model: "nope", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("result = %+v", r)
	}
}
