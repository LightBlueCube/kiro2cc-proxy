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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
	"agyrelay/internal/tokens"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type harness struct {
	t       *testing.T
	gw      *Gateway
	p       *pool.Pool
	log     string
	records *recordLog
	budget  *fakeBudget
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
	var p *pool.Pool
	cloudCode := httptest.NewServer(modelCallRecorder(t, func() *pool.Pool { return p }))
	t.Cleanup(cloudCode.Close)
	var configs []pool.AccountConfig
	for i := range accounts {
		name := fmt.Sprintf("acct%d", i)
		env := append([]string{agytest.EnvRun + "=1", "FAKE_AGY_LOG=" + logPath}, extraEnv[i]...)
		configs = append(configs, pool.AccountConfig{Name: name, Home: filepath.Join(dir, fmt.Sprintf("home%d", i)), Env: env, CloudCodeEndpoint: cloudCode.URL + "/" + name})
	}
	b, err := bridge.Start(logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err = pool.New(pool.Config{Bin: os.Args[0], Accounts: configs, TurnsPerAccount: 4, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	records := &recordLog{}
	budget := &fakeBudget{spent: map[string]map[string]float64{}, unpriced: map[string]bool{}}
	gw, err := New(Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Recorder: records, Budget: budget, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gw.Close()
		p.Close()
		b.Close()
	})
	return &harness{t: t, gw: gw, p: p, log: logPath, records: records, budget: budget}
}

// modelCallRecorder stands in for the Cloud Code proxy: it records the
// model calls the fake agy of account <name> posts to /<name>.
func modelCallRecorder(t *testing.T, current func() *pool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		var call agytest.ModelCall
		account := current().Account(name)
		if "/"+path != agytest.ModelCallPath || account == nil || json.NewDecoder(r.Body).Decode(&call) != nil {
			t.Errorf("unexpected Cloud Code request %s", r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if call.Code == http.StatusOK {
			account.RecordModelCall(call.Session, nil)
			return
		}
		failure := &pool.ModelFailure{Code: call.Code, Message: call.Message}
		if call.RetryAfter > 0 {
			failure.Reset = time.Now().Add(call.RetryAfter)
		}
		account.RecordModelCall(call.Session, failure)
	})
}

// fakeBudget reports set spend by key and group, prices every model but
// those in unpriced, and counts its calls.
type fakeBudget struct {
	mu       sync.Mutex
	spent    map[string]map[string]float64
	unpriced map[string]bool
	err      error
	calls    int
}

func (b *fakeBudget) set(key, group string, spent float64) {
	b.mu.Lock()
	if b.spent[key] == nil {
		b.spent[key] = map[string]float64{}
	}
	b.spent[key][group] = spent
	b.mu.Unlock()
}

func (b *fakeBudget) Spent(_ context.Context, key string) (map[string]float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.spent[key], b.err
}

func (b *fakeBudget) Priced(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return !b.unpriced[model]
}

type recordLog struct {
	mu   sync.Mutex
	list []Record
}

func (l *recordLog) Record(r Record) {
	l.mu.Lock()
	l.list = append(l.list, r)
	l.mu.Unlock()
}

// all waits until n records arrived; they come after the last event.
func (l *recordLog) all(t *testing.T, n int) []Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		got := slices.Clone(l.list)
		l.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type result struct {
	text      string
	reasoning string
	calls     []conv.ToolCall
	anchor    string
	usage     *conv.Usage
	finish    conv.FinishReason
	err       *conv.Error
}

func (h *harness) complete(ctx context.Context, user string, req conv.Request) result {
	h.t.Helper()
	return h.completeAs(ctx, testCaller(user), req)
}

// testCaller's key hash differs from its name, so mixing them up fails.
func testCaller(name string) conv.Caller { return conv.Caller{KeyHash: "hash-" + name, Name: name} }

func (h *harness) completeAs(ctx context.Context, caller conv.Caller, req conv.Request) result {
	h.t.Helper()
	var r result
	for ev := range h.gw.Complete(ctx, caller, req) {
		r.text += ev.Text
		r.reasoning += ev.Reasoning
		r.calls = append(r.calls, ev.ToolCalls...)
		if ev.Anchor != "" {
			r.anchor = ev.Anchor
		}
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
		// CascadeID and StepIndex are language server parameters.
		CascadeID string `json:"cascadeId"`
		StepIndex int    `json:"stepIndex"`
		Prompt    []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Resource *struct {
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"prompt"`
		Meta struct {
			Antigravity struct {
				SystemPrompt struct {
					Append string `json:"append"`
				} `json:"systemPrompt"`
			} `json:"antigravity"`
		} `json:"_meta"`
	} `json:"params"`
}

// requests returns the logged requests with the given method, or all of
// them for "".
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
		if method == "" || req.Method == method {
			out = append(out, req)
		}
	}
	return out
}

var readTool = conv.ToolSpec{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}

func user(text string) conv.Message { return conv.Message{Role: conv.RoleUser, Text: text} }

func TestInspectAndCloseSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}

	list := h.gw.Sessions()
	if len(list) != 1 || list[0].KeyHash != "hash-alice" || list[0].Turn != nil || list[0].PID == 0 {
		t.Fatalf("sessions = %+v", list)
	}
	info, err := h.p.Accounts()[0].LiveProcs()[0].Inspect()
	if err != nil || info.Resources.RSSBytes == 0 || len(info.Workers) != 1 || info.Workers[0].Sessions[0] != list[0].ID {
		t.Fatalf("inspect = %+v, %v", info, err)
	}
	if err := h.gw.CancelTurn("acct0", list[0].ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cancel without a turn = %v", err)
	}

	if err := h.gw.CloseSession(ctx, "acct0", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.requests("session/close")); n != 1 || len(h.gw.Sessions()) != 0 {
		t.Fatalf("close requests = %d, sessions = %+v", n, h.gw.Sessions())
	}
	// The conversation continues: the session reopens from disk.
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if second.err != nil || second.text != "reply to: again" || len(h.requests("session/resume")) != 1 {
		t.Fatalf("after close = %+v, resumes %d", second, len(h.requests("session/resume")))
	}

	// The janitor closes sessions idle past IdleClose and keeps others.
	h.gw.sweep()
	if n := len(h.requests("session/close")); n != 1 {
		t.Fatalf("a fresh session was closed: %d closes", n)
	}
	h.gw.SetIdleClose(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	h.gw.sweep()
	if n := len(h.requests("session/close")); n != 2 || len(h.gw.Sessions()) != 0 {
		t.Fatalf("idle close: %d closes, sessions %+v", n, h.gw.Sessions())
	}
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: second.text}, user("third"))
	if third := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); third.err != nil || third.text != "reply to: third" {
		t.Fatalf("after idle close = %+v", third)
	}
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new = %d, want 1 (reopened, not replayed)", n)
	}
}

// The janitor asks KeepForQuota before it closes an account's last open
// session, and keeps the session while the answer is yes.
func TestLastSessionWaitsForQuota(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	keep := true
	type ask struct {
		closes   int
		lastUsed time.Time
	}
	var asks []ask
	h.gw.keepForQuota = func(ctx context.Context, account *pool.Account, lastUsed time.Time) bool {
		h.gw.Sessions() // asked without g.mu: this would deadlock
		asks = append(asks, ask{len(h.requests("session/close")), lastUsed})
		return keep
	}
	for _, text := range []string{"first", "second"} {
		if r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(text)}}); r.err != nil {
			t.Fatal(r.err)
		}
	}
	newest := slices.MaxFunc(h.gw.Sessions(), func(a, b SessionInfo) int { return a.LastUsed.Compare(b.LastUsed) })
	h.gw.SetIdleClose(time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	h.gw.sweep()
	h.gw.sweep()
	sessions := h.gw.Sessions()
	if n := len(h.requests("session/close")); n != 1 || len(sessions) != 1 || sessions[0].ID != newest.ID || len(asks) != 2 {
		t.Fatalf("kept: %d closes, sessions %+v, asks %+v", n, sessions, asks)
	}
	if asks[0].closes != 0 || !asks[0].lastUsed.Equal(newest.LastUsed) {
		t.Fatalf("first ask = %+v, want before any close, last used %v", asks[0], newest.LastUsed)
	}
	keep = false
	h.gw.sweep()
	if n := len(h.requests("session/close")); n != 2 || len(h.gw.Sessions()) != 0 || len(asks) != 3 {
		t.Fatalf("released: %d closes, %d sessions, %d asks", n, len(h.gw.Sessions()), len(asks))
	}
}

func TestKeyLimits(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}}

	if r := h.complete(ctx, "bob", req); r.err != nil || h.budget.calls != 0 {
		t.Fatalf("unlimited key: %+v, budget calls %d", r, h.budget.calls)
	}

	// A group limit counts that group's spend only.
	alice := testCaller("alice")
	alice.Limits = map[string]float64{"gemini": 3}
	h.budget.set(alice.KeyHash, "gemini", 2.99)
	h.budget.set(alice.KeyHash, "claude-gpt", 50)
	bob := testCaller("bob")
	bob.Limits = map[string]float64{"gemini": 0.0003}
	h.budget.set(bob.KeyHash, "gemini", 0.000528)
	if r := h.completeAs(ctx, bob, req); r.err == nil || !strings.Contains(r.err.Message, "spent $0.0005 of its $0.0003 limit") {
		t.Fatalf("sub-dollar amounts: %+v", r.err)
	}
	if r := h.completeAs(ctx, alice, req); r.err != nil {
		t.Fatalf("under the group limit: %+v", r)
	}
	h.budget.set(alice.KeyHash, "gemini", 3)
	r := h.completeAs(ctx, alice, req)
	if r.err == nil || r.err.Kind != conv.ErrKeyLimit || !strings.Contains(r.err.Message, "spent $3.00 of its $3.00 limit on gemini models") {
		t.Fatalf("at the group limit: %+v", r.err)
	}

	// A total limit counts every group.
	carol := testCaller("carol")
	total := 1.0
	carol.LimitTotal = &total
	h.budget.set(carol.KeyHash, "claude-gpt", 0.5)
	if r := h.completeAs(ctx, carol, req); r.err != nil {
		t.Fatalf("under the total limit: %+v", r)
	}
	h.budget.set(carol.KeyHash, "gemini", 0.5)
	r = h.completeAs(ctx, carol, req)
	if r.err == nil || r.err.Kind != conv.ErrKeyLimit || !strings.Contains(r.err.Message, "spent $1.00 of its $1.00 limit (at API list prices)") {
		t.Fatalf("at the total limit: %+v", r.err)
	}
	if n := len(h.requests("session/prompt")); n != 3 {
		t.Fatalf("prompts = %d, want 3 (refused requests reach no agy)", n)
	}

	// An unpriced model is refused to keys whose limit covers its group.
	h.budget.unpriced["gemini-3.8-flash"] = true
	dave := testCaller("dave")
	dave.Limits = map[string]float64{"claude-gpt": 1}
	if r := h.completeAs(ctx, dave, req); r.err != nil {
		t.Fatalf("unpriced model outside the limited group: %+v", r.err)
	}
	r = h.completeAs(ctx, alice, req)
	if r.err == nil || r.err.Kind != conv.ErrForbidden || !strings.Contains(r.err.Message, `model "gemini-3.8-flash" has no configured price`) ||
		strings.Contains(r.err.Message, "agyrelay") {
		t.Fatalf("unpriced model: %+v", r.err)
	}
	delete(h.budget.unpriced, "gemini-3.8-flash")

	h.budget.err = errors.New("disk on fire")
	if r := h.completeAs(ctx, alice, req); r.err == nil || r.err.Kind != conv.ErrUnavailable {
		t.Fatalf("budget failure: %+v", r.err)
	}
}

func TestContextWindow(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	long := strings.Repeat("word ", 2000)
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(long)}}

	small := []ContextWindow{{Model: "gemini-3.8-flash", Tokens: 2000}}
	set := func(windows []ContextWindow, check bool) {
		t.Helper()
		if err := h.gw.SetContextWindows(windows, check); err != nil {
			t.Fatal(err)
		}
	}

	// The check is off by default.
	if r := h.complete(ctx, "carol", req); r.err != nil {
		t.Fatalf("check off: %+v", r.err)
	}

	set(small, true)
	r := h.complete(ctx, "alice", req)
	if r.err == nil || r.err.Kind != conv.ErrContextLength || !strings.Contains(r.err.Message, "maximum context length is 2000 tokens") {
		t.Fatalf("over the window: %+v", r.err)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("prompts = %d, want 1 (the refused request reaches no agy)", n)
	}

	// Under the window, without one, and with the check off, the request runs.
	set([]ContextWindow{{Model: "gemini-3.8-flash", Tokens: 10000}}, true)
	if r := h.complete(ctx, "alice", req); r.err != nil {
		t.Fatalf("under the window: %+v", r.err)
	}
	set(nil, true)
	if r := h.complete(ctx, "bob", req); r.err != nil {
		t.Fatalf("no window: %+v", r.err)
	}
	set(small, false)
	if r := h.complete(ctx, "dave", req); r.err != nil {
		t.Fatalf("check turned off: %+v", r.err)
	}
}

func TestPromptEstimate(t *testing.T) {
	set, err := tokens.Load()
	if err != nil {
		t.Fatal(err)
	}
	e := newPromptEstimator(set)
	messages := []conv.Message{{Role: conv.RoleSystem, Text: "be brief"}, user("hello"),
		{Role: conv.RoleAssistant, ToolCalls: []conv.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: conv.RoleTool, ToolCallID: "c1", ToolName: "read", Text: strings.Repeat("package main\n", 100)},
		{Role: conv.RoleUser, Text: "look", Images: []conv.Image{{Data: "AAAA", MimeType: "image/png"}}}}
	req := conv.Request{Messages: messages, Tools: []conv.ToolSpec{{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	tr := newTranscript(messages)

	gemma := set.For("gemini-3.8-flash")
	want := agyPromptTokens + gemma.Image("AAAA") + toolsetTokens + toolDeclarationTokens
	for _, text := range []string{"be brief", renderMessage(messages[1], -1), renderMessage(messages[2], -1),
		renderMessage(messages[3], -1), renderMessage(messages[4], -1), "read\nRead a file\n{\"type\":\"object\"}"} {
		n, err := gemma.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		want += n
	}
	for range 2 { // the second estimate comes from the cache
		got, err := e.estimate("gemini-3.8-flash", req, tr)
		if err != nil || got != want {
			t.Fatalf("estimate = %d, %v; want %d", got, err, want)
		}
	}
	claude, err := e.estimate("claude-opus-5-5", req, tr)
	if err != nil || claude <= want {
		t.Fatalf("claude estimate = %d, %v; want above gemini's %d", claude, err, want)
	}
}

func TestPlainReplyThenWarmDelta(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{{Role: conv.RoleSystem, Text: "be brief"}, user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil || first.finish != conv.FinishStop || first.text != "reply to: hello" {
		t.Fatalf("first = %+v", first)
	}
	if first.usage == nil || *first.usage != oneCall {
		t.Fatalf("usage = %+v, want %+v", first.usage, oneCall)
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

// oneCall is the usage of one model call of the fake agy.
var oneCall = conv.Usage{
	PromptTokens:     agytest.StepInput + agytest.StepCached,
	CompletionTokens: agytest.StepOutput,
	ReasoningTokens:  agytest.StepThought,
	CachedTokens:     agytest.StepCached,
}

// Each response of an agent loop reports its own model calls, not the
// turn's sum, so clients see the context size.
func TestToolLoopResponsesReportTheirOwnCalls(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	var messages []conv.Message
	for turn := range 2 {
		messages = append(messages, user(`@tool read {}`))
		call := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
		if call.finish != conv.FinishToolCalls || call.usage == nil || *call.usage != oneCall {
			t.Fatalf("turn %d tool-call response: finish %q usage %+v, want %+v", turn, call.finish, call.usage, oneCall)
		}
		messages = append(messages,
			conv.Message{Role: conv.RoleAssistant, ToolCalls: call.calls},
			conv.Message{Role: conv.RoleTool, ToolCallID: call.calls[0].ID, Text: "x"})
		done := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
		if done.finish != conv.FinishStop || done.usage == nil || *done.usage != oneCall {
			t.Fatalf("turn %d final response: finish %q usage %+v, want %+v (the turn's total less the first call)", turn, done.finish, done.usage, oneCall)
		}
		messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: done.text})
	}
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new count = %d, want 1 (the second turn runs on the same session)", n)
	}
}

// A position the client left mid-turn has no step count, so the next turn
// launches knowing only a lower bound; its tool-call response still counts
// only its own calls.
func TestToolCallUsageSkipsEarlierTurns(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"
	messages := []conv.Message{user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})

	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("@hang"))
	hangCtx, cancel := context.WithCancel(ctx)
	done := make(chan result)
	go func() {
		done <- h.complete(hangCtx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.requests("session/prompt")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second prompt never sent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	messages = append(messages, user(`@tool read {}`))
	call := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	if call.finish != conv.FinishToolCalls || call.usage == nil || *call.usage != oneCall {
		t.Fatalf("tool-call response: finish %q usage %+v, want %+v", call.finish, call.usage, oneCall)
	}
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new count = %d, want 1 (the turns share a session)", n)
	}

	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: call.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: call.calls[0].ID, Text: "x"})
	end := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	if end.finish != conv.FinishStop || end.usage == nil || *end.usage != oneCall {
		t.Fatalf("final response: finish %q usage %+v, want %+v", end.finish, end.usage, oneCall)
	}
}

func TestPruneDeletesSessionsNobodyCanContinue(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatal(first.err)
	}
	account := h.p.Accounts()[0]
	sessionID := h.gw.Sessions()[0].ID
	dir := filepath.Join(account.GeminiDir(), "antigravity-cli")
	old := time.Now().Add(-2 * conversationGrace)
	age := func(path string) {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "conversations", "probe.db"))
	write(filepath.Join(dir, "brain", "probe", "scratch", "x"))
	age(filepath.Join(dir, "conversations", "probe.db"))
	write(filepath.Join(dir, "conversations", "starting.db"))
	age(account.ConversationStore(sessionID))
	exists := func(id string) bool {
		_, err := os.Stat(filepath.Join(dir, "conversations", id+".db"))
		return err == nil
	}

	h.gw.pruneConversations()
	if exists("probe") || !exists("starting") || !exists(sessionID) {
		t.Fatalf("after prune: probe %v, starting %v, open session %v", exists("probe"), exists("starting"), exists(sessionID))
	}
	if _, err := os.Stat(filepath.Join(dir, "brain", "probe")); err == nil {
		t.Fatal("the probe's brain directory stayed")
	}

	// Closed, its snapshot still lets alice continue it.
	if err := h.gw.CloseSession(ctx, account.Name(), sessionID); err != nil {
		t.Fatal(err)
	}
	h.gw.pruneConversations()
	if !exists(sessionID) {
		t.Fatal("a session with a live snapshot was deleted")
	}

	// Past the TTL and the grace, nobody can continue it.
	h.gw.snaps.mu.Lock()
	for ref, positions := range h.gw.snaps.bySession {
		for i := range positions {
			positions[i].UpdatedAt = time.Now().Add(-time.Hour - 2*conversationGrace)
		}
		h.gw.snaps.bySession[ref] = positions
	}
	h.gw.snaps.mu.Unlock()
	h.gw.pruneConversations()
	if exists(sessionID) {
		t.Fatal("a session past its snapshot's expiry stayed")
	}
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	if second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); second.err != nil {
		t.Fatal(second.err)
	}
	if n := len(h.requests("session/new")); n != 2 {
		t.Fatalf("session/new count = %d, want 2 (replayed)", n)
	}
}

func TestSystemMessagePlacement(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{{Role: conv.RoleSystem, Text: "be brief"}, {Role: conv.RoleSystem, Text: "be kind"}, user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatal(first.err)
	}
	opened := h.requests("session/new")
	if got := opened[0].Params.Meta.Antigravity.SystemPrompt.Append; !strings.HasSuffix(got, "\n\nbe brief\n\nbe kind") {
		t.Fatalf("append = %q, want the leading system messages at its end", got)
	}
	if prompt := h.requests("session/prompt")[0].Params.Prompt[0].Text; prompt != "hello" {
		t.Fatalf("fresh prompt = %q, want only the user input", prompt)
	}

	// A later system message stays at its position and keeps the session warm.
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text},
		conv.Message{Role: conv.RoleSystem, Text: "answer in French"}, user("again"))
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if second.err != nil {
		t.Fatal(second.err)
	}
	delta := h.requests("session/prompt")[1].Params.Prompt[0].Resource
	if n := len(h.requests("session/new")); n != 1 || delta == nil || !strings.Contains(delta.Text, "<system>\nanswer in French\n</system>\n\n<user>\nagain\n</user>") {
		t.Fatalf("session/new count = %d, delta = %+v; want a warm delta with the system message in place", n, delta)
	}

	// A different leading system prompt needs a session with that prompt.
	messages[0].Text = "be terse"
	if third := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); third.err != nil {
		t.Fatal(third.err)
	}
	opened = h.requests("session/new")
	if len(opened) != 2 || !strings.HasSuffix(opened[1].Params.Meta.Antigravity.SystemPrompt.Append, "\n\nbe terse\n\nbe kind") {
		t.Fatalf("session/new = %+v, want a second session with the new system prompt", opened)
	}
	replay := h.requests("session/prompt")[2].Params.Prompt[0].Text
	if !strings.Contains(replay, "<system>\nanswer in French\n</system>") || strings.Contains(replay, "be terse") {
		t.Fatalf("replay = %q, want the later system message in the history only", replay)
	}
}

func TestHistoryNamesToolsAsTheModelCallsThem(t *testing.T) {
	call := conv.Message{Role: conv.RoleAssistant, ToolCalls: []conv.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}}}
	result := conv.Message{Role: conv.RoleTool, ToolCallID: "c1", ToolName: "read", Text: "x"}
	orphan := conv.Message{Role: conv.RoleTool, ToolCallID: "c0", Text: "y"}
	got, _ := renderHistory([]conv.Message{call, result, orphan})
	for _, want := range []string{"<assistant-tool-call name=\"mcp_client_read\" id=\"c1\">\n{\"path\":\"a.go\",\"toolSummary\":\"\"}\n",
		`<tool-result name="mcp_client_read" id="c1">`, `<tool-result name="" id="c0">`} {
		if !strings.Contains(got, want) {
			t.Errorf("history lacks %s:\n%s", want, got)
		}
	}
}

// A replayed conversation keeps the images of its history, numbered in the
// text and attached ahead of the newest input's own.
func TestFreshPromptCarriesHistoryImages(t *testing.T) {
	image := func(data string) []conv.Image { return []conv.Image{{Data: data, MimeType: "image/png"}} }
	messages := []conv.Message{
		{Role: conv.RoleUser, Text: "first", Images: image("AAAA")},
		{Role: conv.RoleAssistant, ToolCalls: []conv.ToolCall{{ID: "c1", Name: "shot", Arguments: json.RawMessage(`{}`)}}},
		{Role: conv.RoleTool, ToolCallID: "c1", ToolName: "shot", Text: "done", Images: image("BBBB")},
		{Role: conv.RoleAssistant, Text: "seen"},
		{Role: conv.RoleUser, Text: "newest", Images: image("CCCC")},
	}
	blocks := freshPrompt(newTranscript(messages))

	var got []string
	for _, block := range blocks[:len(blocks)-1] {
		got = append(got, block.Type+":"+block.Data)
	}
	if want := []string{"image:AAAA", "image:BBBB", "image:CCCC"}; !slices.Equal(got, want) {
		t.Fatalf("blocks before the text = %v, want %v", got, want)
	}
	text := blocks[len(blocks)-1].Text
	for _, want := range []string{"first\n[Attached image 1]", "done\n[Attached image 2]", "\n\nnewest"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, historyPlaceholder) {
		t.Errorf("history images still replaced by a placeholder:\n%s", text)
	}
}

func TestParallelAcceptsPrefixedNames(t *testing.T) {
	known := func(name string) bool { return name == "read" }
	calls, err := parseParallel(json.RawMessage(`{"calls":[{"name":"mcp_client_read"},{"name":"read"}]}`), known)
	if err != nil || calls[0].Name != "read" || calls[1].Name != "read" {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
	if _, err := parseParallel(json.RawMessage(`{"calls":[{"name":"mcp_client_parallel"}]}`), known); err == nil {
		t.Fatal("nested parallel accepted")
	}
}

// Models copy agy's toolSummary into a batch's calls; the client never
// declared it.
func TestParallelDropsToolSummary(t *testing.T) {
	known := func(name string) bool { return name == "read" }
	calls, err := parseParallel(json.RawMessage(`{"calls":[{"name":"read","arguments":{"path":"a.go","toolSummary":"Read a.go","n":12345678901234567890}},{"name":"read","arguments":{"path":"b.go"}}]}`), known)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(calls[0].Arguments); got != `{"n":12345678901234567890,"path":"a.go"}` {
		t.Errorf("first arguments = %s", got)
	}
	if got := string(calls[1].Arguments); got != `{"path":"b.go"}` {
		t.Errorf("second arguments = %s, want them untouched", got)
	}
}

func TestToolResultCarriesLaterSystemMessage(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	messages := []conv.Message{user(`@tool read {}`)}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Text: "contents"},
		conv.Message{Role: conv.RoleSystem, Text: "the file changed"},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err != nil || !strings.Contains(second.text, "<system>\nthe file changed\n</system>") {
		t.Fatalf("second = %+v, want the system message on the tool result", second)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("session/prompt count = %d, want 1 (same turn)", n)
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

func TestQuotaFailureRetriesOnAnotherAccount(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	name := userPreferring(t, h.p, "acct0")
	r := h.complete(context.Background(), name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err != nil || r.text != "reply to: hi" {
		t.Fatalf("result = %+v", r)
	}
	if err := h.p.Account("acct0").Available("gemini"); err == nil {
		t.Fatal("acct0 is not cooling down")
	}
}

// userPreferring finds a user whose first-choice account is account.
func userPreferring(t *testing.T, p *pool.Pool, account string) string {
	t.Helper()
	for i := range 100 {
		candidate := fmt.Sprintf("user%d", i)
		picked, err := p.Pick(candidate, "gemini", nil)
		if err != nil {
			t.Fatal(err)
		}
		picked.Release()
		if picked.Name() == account {
			return candidate
		}
	}
	t.Fatalf("no user prefers %s", account)
	return ""
}

// Accounts offer different models: requests go to an account that has
// the model, with that account's tiers, and the catalog is their union.
func TestModelsRouteToAccountsOfferingThem(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{
		0: {"FAKE_AGY_MODELS=gemini-3.8-flash-low,gemini-3.8-flash-high"},
		1: {"FAKE_AGY_MODELS=gemini-3.8-flash-medium,claude-opus-5-5-high"},
	})
	ctx := context.Background()
	alice := userPreferring(t, h.p, "acct0")
	request := func(model string, messages ...conv.Message) result {
		return h.complete(ctx, alice, conv.Request{Model: model, Messages: messages})
	}

	// No catalog is reported yet: acct0, alice's first choice, reports it
	// has no claude when its session opens, so acct1 serves the request at
	// the tier its own catalog has.
	if r := request("claude-opus-5-5", user("c")); r.err != nil {
		t.Fatalf("claude = %+v", r.err)
	}
	if got := h.records.all(t, 1)[0]; got.Account != "acct1" || got.Slug != "claude-opus-5-5-high" {
		t.Fatalf("claude record = %+v", got)
	}

	// Both reported now; acct0 lacks claude, acct1 lacks gemini low.
	first := request("gemini-3.8-flash", user("one"))
	if first.err != nil {
		t.Fatalf("gemini = %+v", first.err)
	}
	ids := []string{}
	for _, m := range h.gw.Models() {
		ids = append(ids, m.ID+fmt.Sprint(m.Efforts))
	}
	if want := []string{"gemini-3.8-flash[low medium high]", "claude-opus-5-5[high]"}; !slices.Equal(ids, want) {
		t.Fatalf("models = %v, want %v", ids, want)
	}
	if r := request("gemini-3.8-flash-medium", user("m")); r.err != nil {
		t.Fatalf("pinned medium = %+v", r.err)
	}

	// The conversation lives on acct0; switching it to claude replays it
	// on acct1 instead of failing there.
	messages := []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two")}
	if r := request("claude-opus-5-5", messages...); r.err != nil {
		t.Fatalf("switch = %+v", r.err)
	}
	records := h.records.all(t, 4)
	want := []struct{ account, slug string }{{"acct1", "claude-opus-5-5-high"}, {"acct0", "gemini-3.8-flash-low"}, {"acct1", "gemini-3.8-flash-medium"}, {"acct1", "claude-opus-5-5-high"}}
	for i, w := range want {
		if records[i].Account != w.account || records[i].Slug != w.slug {
			t.Errorf("record %d = %s %s, want %s %s", i, records[i].Account, records[i].Slug, w.account, w.slug)
		}
	}
	if records[3].Mode != ModeReplay {
		t.Errorf("switch mode = %q, want replay", records[3].Mode)
	}

	if r := request("gpt-oss-120b", user("x")); r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("model no account offers = %+v", r)
	}
}

func TestXHighAndMaxEffortTiers(t *testing.T) {
	entries := entriesFromACP([]agy.ModelInfo{
		{ModelID: "gemini-4-hydrogen-low", Name: "Gemini 4 Hydrogen (Low)"},
		{ModelID: "gemini-4-hydrogen-medium", Name: "Gemini 4 Hydrogen (Medium)"},
		{ModelID: "gemini-4-hydrogen-high", Name: "Gemini 4 Hydrogen (High)"},
		{ModelID: "gemini-4-hydrogen-xhigh", Name: "Gemini 4 Hydrogen (XHigh)"},
		{ModelID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)"},
		{ModelID: "claude-opus-5-5-max", Name: "Claude Opus 5.5 (Max)"},
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].id != "gemini-4-hydrogen" || entries[0].name != "Gemini 4 Hydrogen" ||
		!slices.Equal(entries[0].efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("hydrogen = %+v", entries[0])
	}
	if entries[1].id != "claude-opus-5-5" || entries[1].name != "Claude Opus 5.5" ||
		!slices.Equal(entries[1].efforts, []string{"high", "max"}) {
		t.Fatalf("opus = %+v", entries[1])
	}

	cases := []struct {
		model, effort string
		wantSlug      string
		wantTier      string
	}{
		{"gemini-4-hydrogen", "high", "gemini-4-hydrogen-high", "high"},
		{"gemini-4-hydrogen", "xhigh", "gemini-4-hydrogen-xhigh", "xhigh"},
		{"gemini-4-hydrogen", "max", "gemini-4-hydrogen-xhigh", "xhigh"},
		{"gemini-4-hydrogen-xhigh", "", "gemini-4-hydrogen-xhigh", "xhigh"},
		{"claude-opus-5-5", "xhigh", "claude-opus-5-5-max", "max"},
		{"claude-opus-5-5", "max", "claude-opus-5-5-max", "max"},
		{"claude-opus-5-5-max", "", "claude-opus-5-5-max", "max"},
	}
	for _, tc := range cases {
		choice, ok := lookupModel(entries, tc.model, tc.effort)
		if !ok {
			t.Fatalf("lookup(%q, %q) failed", tc.model, tc.effort)
		}
		slug, tier, ok := choice.slugIn(entries)
		if !ok || slug != tc.wantSlug || tier != tc.wantTier {
			t.Fatalf("slugIn(%q, %q) = (%q, %q, %v), want (%q, %q, true)",
				tc.model, tc.effort, slug, tier, ok, tc.wantSlug, tc.wantTier)
		}
	}
}

// Accounts without a catalog are refreshed before the request is routed,
// once for concurrent requests; one whose refresh fails is not refreshed
// again right away and is tried as before.
func TestRequestRefreshesUnreportedAccounts(t *testing.T) {
	catalogs := map[string]string{"acct0": "gemini-3.8-flash-low", "acct1": "claude-opus-5-5-high"}
	h := newHarness(t, 3, map[int][]string{
		0: {"FAKE_AGY_MODELS=" + catalogs["acct0"]},
		1: {"FAKE_AGY_MODELS=" + catalogs["acct1"]},
		2: {"FAKE_AGY_MODELS=gemini-3.8-flash-low"}, // its refresh fails
	})
	var mu sync.Mutex
	calls := map[string]int{}
	h.gw.refresh = func(ctx context.Context, account *pool.Account) error {
		mu.Lock()
		calls[account.Name()]++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // let the requests meet in it
		ids, ok := catalogs[account.Name()]
		if !ok {
			return errors.New("refresh failed")
		}
		account.SetModels([]agy.ModelInfo{{ModelID: ids, Name: ids}})
		return nil
	}
	alice := userPreferring(t, h.p, "acct0")

	var wg sync.WaitGroup
	results := make([]result, 3)
	for i := range results {
		wg.Go(func() {
			results[i] = h.complete(context.Background(), alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user(fmt.Sprint("c", i))}})
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d = %+v", i, r.err)
		}
	}
	for _, rec := range h.records.all(t, 3) {
		if rec.Account != "acct1" {
			t.Errorf("claude went to %s", rec.Account)
		}
	}
	// acct0's catalog was known before routing: no session was opened there.
	if st := h.p.Account("acct0").Status(); st.Procs != 0 {
		t.Errorf("acct0 started agy: %+v", st)
	}

	if r := h.complete(context.Background(), alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("g")}}); r.err != nil {
		t.Fatalf("gemini = %+v", r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := map[string]int{"acct0": 1, "acct1": 1, "acct2": 1}; !maps.Equal(calls, want) {
		t.Errorf("refreshes = %v, want %v", calls, want)
	}
}

// A model on the fallback list that the account's catalog turns out not to
// have is an unknown model, not an unavailable account.
func TestFallbackModelMissingFromCatalogIsUnknown(t *testing.T) {
	h := newHarness(t, 1, nil)
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gpt-oss-120b", Messages: []conv.Message{user("x")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("result = %+v", r)
	}
	if r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("y")}}); r.err != nil {
		t.Fatalf("after = %+v", r.err)
	}
}

func TestQuotaFailureWithoutFallbackIs429(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	wantWait := int(agytest.QuotaRetryAfter.Seconds())
	if r.err == nil || r.err.Kind != conv.ErrQuota || r.err.Message != agytest.QuotaError || r.err.RetryAfterSeconds != wantWait || r.text != "" {
		t.Fatalf("result = %+v, want quota error retrying after %d s", r, wantWait)
	}
	// While every account offering the model is cooling down, later
	// requests get a non-retryable ErrUnknownModel instead of 503.
	again := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if again.err == nil || again.err.Kind != conv.ErrUnknownModel ||
		strings.Contains(again.err.Message, "acct0") || strings.Contains(again.err.Message, "cooling down") {
		t.Fatalf("cooling down = %+v, want ErrUnknownModel without pool details", again)
	}
}

func TestModelOverflowIsContextLengthError(t *testing.T) {
	h := newHarness(t, 1, nil)
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@overflow")}})
	if r.err == nil || r.err.Kind != conv.ErrContextLength || r.err.Message != agytest.OverflowError || r.text != "" {
		t.Fatalf("result = %+v", r)
	}
}

// Once agy recovers from a failed model call, what it said about the
// failure stays in the reply as it was.
func TestRecoveredModelCallIsReply(t *testing.T) {
	h := newHarness(t, 1, nil)
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@retry429")}})
	if want := "\n" + agytest.RetryNotice + "\nretried"; r.err != nil || r.text != want {
		t.Fatalf("result = %+v, want text %q", r, want)
	}
}

// A failure left by an abandoned turn does not fail the next turn.
func TestEarlierTurnFailureIsForgotten(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("a")}})
	if first.err != nil {
		t.Fatal(first.err)
	}
	session := h.requests("session/prompt")[0].Params.SessionID
	h.p.Account("acct0").RecordModelCall(session, &pool.ModelFailure{Code: http.StatusInternalServerError, Message: "stale"})
	messages := []conv.Message{user("a"), {Role: conv.RoleAssistant, Text: first.text}, user("b")}
	r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if r.err != nil || r.text != "reply to: b" {
		t.Fatalf("result = %+v", r)
	}
	if prompts := h.requests("session/prompt"); len(prompts) != 2 || prompts[1].Params.SessionID != session {
		t.Fatal("second turn did not continue the session")
	}
}

// A failure no turn read, as of a call cancelled when the client left,
// goes with its session.
func TestClosedSessionForgetsItsFailure(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	if r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("a")}}); r.err != nil {
		t.Fatal(r.err)
	}
	account := h.p.Account("acct0")
	session := h.requests("session/prompt")[0].Params.SessionID
	account.RecordModelCall(session, &pool.ModelFailure{Code: http.StatusBadGateway, Message: "Cloud Code request failed: context canceled"})

	h.gw.SetIdleClose(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	h.gw.sweep()
	if len(h.requests("session/close")) != 1 {
		t.Fatal("idle session not closed")
	}
	if failure := account.ModelFailure(session); failure != nil {
		t.Fatalf("closed session keeps %+v", failure)
	}
}

// Thinking can last long before any text; the client sees it as it comes.
func TestReasoningStreamsBeforeText(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := h.gw.Complete(ctx, testCaller("alice"), conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@think-hang")}})
	select {
	case ev := <-events:
		if ev.Reasoning != "thinking" {
			t.Fatalf("first event = %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reasoning held back")
	}
	cancel()
	for range events {
	}
}

func TestDisconnectCancelsTurn(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result)
	go func() {
		done <- h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@hang")}})
	}()
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

func TestAnchorLetsAnotherKeyContinue(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil || first.anchor == "" {
		t.Fatalf("first = %+v", first)
	}
	if _, ok := parseAnchor(first.anchor); !ok {
		t.Fatalf("anchor %q does not parse", first.anchor)
	}

	// Without the anchor bob cannot reach alice's session.
	reply := conv.Message{Role: conv.RoleAssistant, Text: first.text, Anchors: []string{"gAAAA-other-provider", "agr1.!!!"}}
	bob := h.complete(ctx, "bob", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello"), reply, user("more")}})
	if bob.err != nil || len(h.requests("session/new")) != 2 {
		t.Fatalf("bob without anchor = %+v, sessions %d", bob, len(h.requests("session/new")))
	}

	// With it he continues the session warm, wherever the anchor sits.
	carol := []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")}
	carol[0].Anchors = []string{first.anchor}
	r := h.complete(ctx, "carol", conv.Request{Model: "gemini-3.8-flash", Messages: carol})
	if r.err != nil || r.text != "reply to: again" {
		t.Fatalf("carol = %+v", r)
	}
	prompts := h.requests("session/prompt")
	if len(h.requests("session/new")) != 2 || prompts[len(prompts)-1].Params.SessionID != prompts[0].Params.SessionID {
		t.Fatal("anchored request did not reuse alice's session")
	}
}

func TestUnknownModel(t *testing.T) {
	h := newHarness(t, 1, nil)
	r := h.complete(context.Background(), "alice", conv.Request{Model: "nope", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("result = %+v", r)
	}
}

// assertRewound checks that the last revert kept keep steps of the session
// and that session/resume followed it before the next prompt.
func (h *harness) assertRewound(sessionID string, keep int) {
	h.t.Helper()
	reverts := h.requests("ls/RevertToCascadeStep")
	if len(reverts) == 0 {
		h.t.Fatal("no revert")
	}
	last := reverts[len(reverts)-1]
	if last.Params.CascadeID != sessionID || last.Params.StepIndex != keep-1 {
		h.t.Fatalf("revert = %+v, want session %s stepIndex %d", last.Params, sessionID, keep-1)
	}
	var after []string
	reverted := false
	for _, req := range h.requests("") {
		if req.Method == "ls/RevertToCascadeStep" {
			reverted, after = true, nil
			continue
		}
		if reverted && strings.HasPrefix(req.Method, "session/") {
			after = append(after, req.Method)
		}
	}
	if len(after) < 2 || after[0] != "session/resume" {
		h.t.Fatalf("after revert: %v, want session/resume first", after)
	}
}

func TestRegenerateRewindsSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	messages := []conv.Message{user("one")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("two"))
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil || second.err != nil {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}

	// The client regenerates the second reply with another input.
	retry := append(slices.Clone(messages[:2]), user("@steps"))
	r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: retry})
	if r.err != nil || r.text != "steps: 2" {
		t.Fatalf("regenerated = %+v, want the session rewound to 2 steps", r)
	}
	sessionID := h.requests("session/prompt")[0].Params.SessionID
	h.assertRewound(sessionID, 2)
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new count = %d, want 1", n)
	}

	// The discarded branch's own position is gone; continuing it rewinds
	// to the shared one again and sends the rest as delta.
	discarded := append(slices.Clone(messages), conv.Message{Role: conv.RoleAssistant, Text: second.text}, user("three"))
	if r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: discarded}); r.err != nil {
		t.Fatal(r.err)
	}
	if n := len(h.requests("ls/RevertToCascadeStep")); n != 2 {
		t.Fatalf("revert count = %d, want 2", n)
	}
	h.assertRewound(sessionID, 2)
	prompts := h.requests("session/prompt")
	delta := prompts[len(prompts)-1].Params.Prompt[0]
	if delta.Resource == nil || !strings.Contains(delta.Resource.Text, "two") {
		t.Fatalf("delta = %+v, want the discarded branch from message 3 on", delta)
	}
}

func TestRewindToEndOfToolTurn(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	messages := []conv.Message{user(`@tool read {}`)}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, Text: "x"},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: second.text})
	third := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: append(slices.Clone(messages), user("next")), Tools: tools})
	if first.err != nil || second.err != nil || third.err != nil {
		t.Fatalf("setup: %+v %+v %+v", first, second, third)
	}

	// user, planner with the call, tool result, planner with the answer.
	r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: append(messages, user("@steps")), Tools: tools})
	if r.err != nil || r.text != "steps: 4" {
		t.Fatalf("regenerated = %+v", r)
	}
	h.assertRewound(h.requests("session/prompt")[0].Params.SessionID, 4)
}

// A client resends its tool results while the first attempt still runs.
// The cancelled attempt's results stay in the session, so the retry must
// not send them again on top: it rewinds to the last finished turn.
func TestRetriedToolResultsRewindPastCancelledInput(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	req := func(messages []conv.Message) conv.Request {
		return conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: []conv.ToolSpec{readTool}}
	}
	messages := []conv.Message{user("one")}
	first := h.complete(ctx, "alice", req(messages))
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user(`@tool read {}`))
	parked := h.complete(ctx, "alice", req(messages))
	if first.err != nil || parked.err != nil || len(parked.calls) != 1 {
		t.Fatalf("setup: %+v, %+v", first, parked)
	}
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: parked.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: parked.calls[0].ID, Text: "@hang"},
	)

	cancelled := make(chan result)
	go func() { cancelled <- h.complete(ctx, "alice", req(messages)) }()
	deadline := time.Now().Add(10 * time.Second)
	for !h.turnIs(TurnRunning) {
		if time.Now().After(deadline) {
			t.Fatal("tool results never continued the turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	retry := h.complete(ctx, "alice", req(messages))
	if r := <-cancelled; r.err == nil {
		t.Fatalf("first attempt = %+v, want cancelled", r)
	}
	if retry.err != nil {
		t.Fatalf("retry = %+v", retry)
	}
	h.assertRewound(h.requests("session/prompt")[0].Params.SessionID, 2)
	records := h.records.all(t, 4)
	if last := records[len(records)-1]; last.Mode != ModeRewind {
		t.Fatalf("retry mode = %q, want %q", last.Mode, ModeRewind)
	}
}

// waitRun waits until acct0 holds turns active turns, and has a running
// one (making model calls) or not. It returns when the oldest began.
func (h *harness) waitRun(turns int, running bool) time.Time {
	h.t.Helper()
	account := h.p.Account("acct0")
	deadline := time.Now().Add(10 * time.Second)
	for {
		since, ok := account.OldestRun()
		if account.Turns() == turns && ok == running {
			return since
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("turns = %d, running = %v; want %d, %v", account.Turns(), ok, turns, running)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cancelTurns cancels every session's turn.
func (h *harness) cancelTurns() {
	h.t.Helper()
	for _, s := range h.gw.Sessions() {
		if s.Turn == nil {
			continue
		}
		if err := h.gw.CancelTurn(s.Account, s.ID); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A turn runs while it makes model calls: not while it waits for tool
// results, though it holds its slot.
func TestParkedTurnIsNotRunning(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"
	parkFor := func(name string) result {
		t.Helper()
		parked := h.complete(ctx, name, conv.Request{Model: model, Messages: []conv.Message{user(`@tool read {}`)}, Tools: tools})
		if parked.err != nil || len(parked.calls) != 1 {
			t.Fatalf("parked = %+v", parked)
		}
		return parked
	}
	resume := func(name string, parked result, toolResult string) result {
		t.Helper()
		return h.complete(ctx, name, conv.Request{Model: model, Tools: tools, Messages: []conv.Message{
			user(`@tool read {}`),
			{Role: conv.RoleAssistant, ToolCalls: parked.calls},
			{Role: conv.RoleTool, ToolCallID: parked.calls[0].ID, Text: toolResult},
		}})
	}

	// A turn runs from the start of its prompt.
	started := time.Now()
	running := make(chan result)
	go func() {
		running <- h.complete(ctx, "dave", conv.Request{Model: model, Messages: []conv.Message{user("@hang")}})
	}()
	if since := h.waitRun(1, true); since.Before(started) {
		t.Fatalf("turn runs since %v, before its request at %v", since, started)
	}
	h.cancelTurns()
	<-running
	h.waitRun(0, false)

	// The tool results resume it; its end frees the slot.
	parked := parkFor("alice")
	h.waitRun(1, false)
	if second := resume("alice", parked, "done"); second.err != nil {
		t.Fatalf("second = %+v", second)
	}
	h.waitRun(0, false)

	// A resumed turn runs from the resume, not from its first start.
	parked = parkFor("bob")
	h.waitRun(1, false)
	resumed := time.Now()
	hung := make(chan result)
	go func() { hung <- resume("bob", parked, "@hang") }()
	if since := h.waitRun(1, true); since.Before(resumed) {
		t.Fatalf("resumed turn runs since %v, before the resume at %v", since, resumed)
	}
	h.cancelTurns()
	<-hung
	h.waitRun(0, false)

	// A turn that ends while parked is not running afterwards either.
	parkFor("carol")
	h.waitRun(1, false)
	h.cancelTurns()
	h.waitRun(0, false)
}

// turnIs reports whether the only session's turn is in state.
func (h *harness) turnIs(state TurnState) bool {
	sessions := h.gw.Sessions()
	return len(sessions) == 1 && sessions[0].Turn != nil && sessions[0].Turn.State == state
}

func TestRecordsDescribeRequests(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	messages := []conv.Message{user(`@tool read {}`)}
	first := h.complete(ctx, "alice", conv.Request{API: "chat", Model: model, Messages: messages, Tools: tools})
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, Text: "x"})
	second := h.complete(ctx, "alice", conv.Request{API: "chat", Model: model, Messages: messages, Tools: tools})
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: second.text})
	h.complete(ctx, "alice", conv.Request{API: "chat", Model: model, Messages: append(slices.Clone(messages), user("a")), Tools: tools})
	h.complete(ctx, "alice", conv.Request{API: "chat", Model: model, Effort: "xhigh", Messages: append(slices.Clone(messages), user("b")), Tools: tools})
	h.complete(ctx, "alice", conv.Request{API: "chat", Model: "nope", Messages: messages})

	records := h.records.all(t, 5)
	if len(records) != 5 {
		t.Fatalf("records = %d", len(records))
	}
	wantModes := []Mode{ModeFresh, ModeContinue, ModeWarm, ModeRewind, ""}
	for i, r := range records {
		if r.Mode != wantModes[i] || r.KeyHash != "hash-alice" || r.KeyName != "alice" {
			t.Errorf("record %d: mode %q key %q %q, want %q", i, r.Mode, r.KeyHash, r.KeyName, wantModes[i])
		}
	}
	tool, done := records[0], records[1]
	if tool.Finish != conv.FinishToolCalls || tool.Usage == nil || *tool.Usage != oneCall || tool.Account != "acct0" || tool.Model != model || tool.QuotaGroup != "gemini" {
		t.Errorf("tool-call record = %+v", tool)
	}
	// The fake offers low and high: no effort picks the first.
	if tool.Effort != "low" || tool.Slug != model+"-low" || records[3].Effort != "high" || records[3].Slug != model+"-high" {
		t.Errorf("efforts = %q (slug %q), %q (slug %q), want low, high", tool.Effort, tool.Slug, records[3].Effort, records[3].Slug)
	}
	if done.Finish != conv.FinishStop || done.Usage == nil || *done.Usage != oneCall || done.TimeToFirstOutput <= 0 || done.Duration < done.TimeToFirstOutput {
		t.Errorf("final record = %+v", done)
	}
	if bad := records[4]; bad.Err == nil || bad.Err.Kind != conv.ErrUnknownModel || bad.Account != "" {
		t.Errorf("unknown model record = %+v", bad)
	}
}

func TestRecordsNameReplayReasons(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	model := "gemini-3.8-flash"
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user("hello")}})
	history := []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")}
	// bob holds no snapshot of alice's conversation.
	h.complete(ctx, "bob", conv.Request{Model: model, Messages: history})
	// alice's snapshot has no tools.
	h.complete(ctx, "alice", conv.Request{Model: model, Messages: history, Tools: []conv.ToolSpec{readTool}})

	records := h.records.all(t, 3)
	want := []struct {
		mode   Mode
		reason ReplayReason
	}{{ModeFresh, ""}, {ModeReplay, ReplayNoSnapshot}, {ModeReplay, ReplayToolsChanged}}
	for i, r := range records {
		if r.Err != nil || r.Mode != want[i].mode || r.ReplayReason != want[i].reason {
			t.Errorf("record %d: mode %q reason %q err %v, want %+v", i, r.Mode, r.ReplayReason, r.Err, want[i])
		}
	}
}

func TestPositionsKeepOnlyRewindableHistory(t *testing.T) {
	store, err := openSnapshotStore(filepath.Join(t.TempDir(), "snapshots.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	at := func(count, steps int) snapshot {
		return snapshot{User: "u", Account: "a", SessionID: "s", Count: count, Hash: fmt.Sprint("h", count), Steps: steps}
	}
	for _, snap := range []snapshot{at(2, 2), at(4, 4), at(5, 0), at(7, 8)} {
		if err := store.put(snap); err != nil {
			t.Fatal(err)
		}
	}
	counts := func() []int {
		var out []int
		for _, snap := range store.bySession[sessionRef("a", "s")] {
			out = append(out, snap.Count)
		}
		return out
	}
	if got := counts(); !slices.Equal(got, []int{2, 4, 7}) {
		t.Fatalf("positions = %v; the parked one is not rewindable", got)
	}

	if err := store.rewound("a", "s", 2); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.position("a", "s", 4, "h4"); ok {
		t.Fatal("position past the rewind is still known")
	}
	reloaded, err := openSnapshotStore(store.path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.position("a", "s", 2, "h2"); !ok || len(reloaded.byPrefix) != 1 {
		t.Fatalf("reloaded = %+v", reloaded.bySession)
	}
}

func TestAccountPriorityAndAllowedModels(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.p.SetTurnsPerAccount(1)
	ctx := context.Background()
	acct0, acct1 := h.p.Account("acct0"), h.p.Account("acct1")

	// alice prefers acct0 by hash, but acct1 has higher priority.
	alice := userPreferring(t, h.p, "acct0")
	acct1.SetRouting(5, nil)
	first := h.complete(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	if got := h.records.all(t, 1)[0]; got.Account != "acct1" {
		t.Fatalf("first account = %s, want acct1 (higher priority)", got.Account)
	}

	// Park a tool call on acct1 so its 1 turn slot is full; the next
	// new conversation goes to acct0.
	tools := []conv.ToolSpec{readTool}
	parked := h.complete(ctx, "bob", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(`@tool read {}`)}, Tools: tools})
	if parked.err != nil || parked.finish != conv.FinishToolCalls {
		t.Fatalf("parked = %+v", parked)
	}
	overflow := h.complete(ctx, "carol", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("while busy")}})
	if overflow.err != nil {
		t.Fatalf("overflow = %+v", overflow.err)
	}
	// Finish bob's parked turn to free acct1's slot.
	unparked := h.complete(ctx, "bob", conv.Request{
		Model: "gemini-3.8-flash", Tools: tools,
		Messages: []conv.Message{user(`@tool read {}`), {Role: conv.RoleAssistant, ToolCalls: parked.calls}, {Role: conv.RoleTool, ToolCallID: parked.calls[0].ID, Text: "ok"}},
	})
	if unparked.err != nil {
		t.Fatalf("unparked = %+v", unparked.err)
	}
	recs := h.records.all(t, 4)
	if recs[1].Account != "acct1" || recs[2].Account != "acct0" || recs[3].Account != "acct1" {
		t.Fatalf("accounts = %s, %s, %s; want acct1, acct0, acct1", recs[1].Account, recs[2].Account, recs[3].Account)
	}

	// Limit acct1 to gemini-3.8-flash: claude skips acct1 and runs on acct0.
	acct1.SetRouting(5, []string{"gemini-3.8-flash"})
	if r := h.complete(ctx, alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("claude")}}); r.err != nil {
		t.Fatalf("claude = %+v", r.err)
	}
	if got := h.records.all(t, 5)[4]; got.Account != "acct0" {
		t.Fatalf("claude account = %s, want acct0", got.Account)
	}

	// Disallow gemini-3.8-flash on acct1: alice's warm session on acct1
	// replays on acct0 instead.
	acct1.SetRouting(5, []string{"claude-opus-5-5"})
	cont := h.complete(ctx, alice, conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")},
	})
	if cont.err != nil {
		t.Fatalf("continue = %+v", cont.err)
	}
	if got := h.records.all(t, 6)[5]; got.Account != "acct0" || got.Mode != ModeReplay {
		t.Fatalf("disallowed warm = %s %s, want acct0 replay", got.Account, got.Mode)
	}

	// A model disallowed on every account is not offered.
	acct0.SetRouting(0, []string{"gemini-3.8-flash"})
	acct1.SetRouting(5, []string{"gemini-3.8-flash"})
	for _, m := range h.gw.Models() {
		if m.ID == "claude-opus-5-5" {
			t.Fatalf("disallowed model still in Models(): %+v", h.gw.Models())
		}
	}
	if r := h.complete(ctx, alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("no")}}); r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("disallowed on all accounts = %+v, want ErrUnknownModel", r)
	}

	// Only acct1 allows claude-opus-5-5, and its claude-gpt quota is cooling
	// down: no account can serve the model, so the error is ErrUnknownModel.
	acct1.SetRouting(5, nil)
	acct1.Cool("claude-gpt", time.Now().Add(time.Hour))
	r := h.complete(ctx, alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("no")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel ||
		strings.Contains(r.err.Message, "acct0") || strings.Contains(r.err.Message, "acct1") || strings.Contains(r.err.Message, "cooling down") {
		t.Fatalf("exhausted on only offering account = %+v, want ErrUnknownModel without pool details", r)
	}
}

func TestWorkerLimitsAndIdleEviction(t *testing.T) {
	h := newHarness(t, 2, nil)
	ctx := context.Background()

	two := 2
	alice := testCaller("alice")
	alice.MaxWorkers = &two

	c1 := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("c1")}})
	c2 := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("c2")}})
	if c1.err != nil || c2.err != nil || len(h.gw.Sessions()) != 2 {
		t.Fatalf("c1=%+v c2=%+v sessions=%+v", c1.err, c2.err, h.gw.Sessions())
	}

	// Touch c1 so c2 is the oldest idle worker; opening c3 evicts c2.
	c1Warm := h.completeAs(ctx, alice, conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("c1"), {Role: conv.RoleAssistant, Text: c1.text}, user("c1-2")},
	})
	c3 := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("c3")}})
	if c1Warm.err != nil || c3.err != nil {
		t.Fatalf("c1Warm=%+v c3=%+v", c1Warm.err, c3.err)
	}
	if got := h.gw.Sessions(); len(got) != 2 {
		t.Fatalf("sessions after c3 = %+v, want 2 (c2 evicted)", got)
	}

	// c2's snapshot stayed on disk: continuing c2 reopens it (ModeWarm) and
	// evicts the oldest idle worker (c1).
	c2Warm := h.completeAs(ctx, alice, conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("c2"), {Role: conv.RoleAssistant, Text: c2.text}, user("c2-2")},
	})
	if c2Warm.err != nil {
		t.Fatalf("c2Warm = %+v", c2Warm.err)
	}
	if recs := h.records.all(t, 5); recs[4].Mode != ModeWarm {
		t.Fatalf("reopened c2 mode = %s, want warm", recs[4].Mode)
	}

	// Lowering alice's cap to 1 trims her second idle worker at once.
	one := 1
	alice.MaxWorkers = &one
	h.gw.TrimKeyIdle(alice.KeyHash, alice.MaxWorkers)
	if got := h.gw.Sessions(); len(got) != 1 {
		t.Fatalf("sessions after TrimKeyIdle(1) = %+v, want 1", got)
	}

	// Park a turn for alice: her 1 worker is now busy, so a second
	// conversation (fresh or warm) fails with ErrKeyLimit without dropping
	// the warm snapshot, while continuing the parked turn succeeds.
	tools := []conv.ToolSpec{readTool}
	parked := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{user(`@tool read {}`)}})
	if parked.err != nil || parked.finish != conv.FinishToolCalls {
		t.Fatalf("parked = %+v", parked)
	}
	if blocked := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("other")}}); blocked.err == nil || blocked.err.Kind != conv.ErrKeyLimit {
		t.Fatalf("key busy fresh = %+v, want ErrKeyLimit", blocked)
	}
	if blockedWarm := h.completeAs(ctx, alice, conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("c2"), {Role: conv.RoleAssistant, Text: c2.text}, user("c2-2"), {Role: conv.RoleAssistant, Text: c2Warm.text}, user("c2-3")},
	}); blockedWarm.err == nil || blockedWarm.err.Kind != conv.ErrKeyLimit {
		t.Fatalf("key busy warm = %+v, want ErrKeyLimit", blockedWarm)
	}
	cont := h.completeAs(ctx, alice, conv.Request{
		Model: "gemini-3.8-flash", Tools: tools,
		Messages: []conv.Message{user(`@tool read {}`), {Role: conv.RoleAssistant, ToolCalls: parked.calls}, {Role: conv.RoleTool, ToolCallID: parked.calls[0].ID, Text: "ok"}},
	})
	if cont.err != nil {
		t.Fatalf("continue parked = %+v", cont.err)
	}

	// Global worker limit: 1 worker across all keys.
	h.gw.SetMaxWorkers(1)
	bobPark := h.complete(ctx, "bob", conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{user(`@tool read {}`)}})
	if bobPark.err != nil || bobPark.finish != conv.FinishToolCalls {
		t.Fatalf("bobPark = %+v", bobPark)
	}
	if globalBusy := h.complete(ctx, "carol", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}); globalBusy.err == nil || globalBusy.err.Kind != conv.ErrUnavailable ||
		strings.Contains(globalBusy.err.Message, "worker") || strings.Contains(globalBusy.err.Message, "1") {
		t.Fatalf("global busy = %+v, want ErrUnavailable without worker capacity", globalBusy)
	}
	bobDone := h.complete(ctx, "bob", conv.Request{
		Model: "gemini-3.8-flash", Tools: tools,
		Messages: []conv.Message{user(`@tool read {}`), {Role: conv.RoleAssistant, ToolCalls: bobPark.calls}, {Role: conv.RoleTool, ToolCallID: bobPark.calls[0].ID, Text: "ok"}},
	})
	if bobDone.err != nil {
		t.Fatalf("bobDone = %+v", bobDone.err)
	}
	if carol := h.complete(ctx, "carol", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}); carol.err != nil || len(h.gw.Sessions()) != 1 {
		t.Fatalf("carol after bob finished = %+v, sessions=%+v", carol.err, h.gw.Sessions())
	}
}

func TestDownstreamErrorsHideInternalInfo(t *testing.T) {
	forbidden := []string{"acct0", "acct1", "antigravity", "Antigravity", "agy", "agyrelay", "cooling down", "paused", "worker", "verify", "no route"}
	assertClean := func(t *testing.T, msg string) {
		t.Helper()
		for _, term := range forbidden {
			if strings.Contains(msg, term) {
				t.Fatalf("downstream error %q leaked internal term %q", msg, term)
			}
		}
	}

	h := newHarness(t, 2, nil)
	ctx := context.Background()

	// All accounts blocked / paused / needing verification.
	h.p.Account("acct0").SetPaused(true)
	h.p.Account("acct1").SetVerification(true, "https://accounts.google.com/v")
	r := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("all unavailable = %+v", r)
	}
	assertClean(t, r.err.Message)

	// Account blocked with an internal reason; downstream must not see it.
	h.p.Account("acct1").SetVerification(false, "")
	h.p.Account("acct1").SetBlocked(errors.New("no route for acct1"))
	r = h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("blocked = %+v", r)
	}
	assertClean(t, r.err.Message)

	// Context window check error message has no internal software name.
	h.p.Account("acct0").SetPaused(false)
	if err := h.gw.SetContextWindows([]ContextWindow{{Model: "gemini-3.8-flash", Tokens: 5}}, true); err != nil {
		t.Fatal(err)
	}
	r = h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(strings.Repeat("word ", 50))}})
	if r.err == nil || r.err.Kind != conv.ErrContextLength {
		t.Fatalf("context window = %+v", r)
	}
	assertClean(t, r.err.Message)
	if err := h.gw.SetContextWindows(nil, false); err != nil {
		t.Fatal(err)
	}

	// Process killed mid-turn: outcome.Err does not leak process/stderr details.
	h.p.Account("acct1").SetBlocked(nil)
	h.p.Account("acct1").SetPaused(true)
	done := make(chan result, 1)
	go func() {
		done <- h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@hang")}})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.requests("session/prompt")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("prompt never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.p.Account("acct0").Restart()
	r = <-done
	if r.err == nil || r.err.Kind != conv.ErrUpstream {
		t.Fatalf("killed mid-turn = %+v", r)
	}
	assertClean(t, r.err.Message)
}
