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
	"agyrelay/internal/sysprompt"
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
	flags   *fakeFlags
	tokens  *tokenLog
}

// fakeFlags keeps the flagged system prompts in memory.
type fakeFlags struct {
	mu      sync.Mutex
	until   map[string]time.Time
	pinned  map[string]bool
	flagged []FlaggedPrompt
	err     error
}

func (f *fakeFlags) Flagged(fingerprint string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.until[fingerprint], f.pinned[fingerprint]
}

func (f *fakeFlags) Flag(ctx context.Context, p FlaggedPrompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.until[p.Fingerprint] = p.Until
	f.flagged = append(f.flagged, p)
	return f.err
}

func (f *fakeFlags) cancel(fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.until, fingerprint)
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
	var gw *Gateway
	tokens := &tokenLog{}
	cloudCode := httptest.NewServer(modelCallRecorder(t, func() *pool.Pool { return p }, func() *Gateway { return gw }, tokens.add))
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
	flags := &fakeFlags{until: map[string]time.Time{}, pinned: map[string]bool{}}
	gw, err = New(Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Recorder: records, Budget: budget, Flags: flags, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gw.Close()
		p.Close()
		b.Close()
	})
	return &harness{t: t, gw: gw, p: p, log: logPath, records: records, budget: budget, flags: flags, tokens: tokens}
}

// tokenLog keeps the call token the gateway named for each model call.
type tokenLog struct {
	mu   sync.Mutex
	list []string
}

func (l *tokenLog) add(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, token)
}

// modelCallRecorder stands in for the Cloud Code proxy: it records the
// model calls the fake agy of account <name> posts to /<name>.
func modelCallRecorder(t *testing.T, currentPool func() *pool.Pool, currentGateway func() *Gateway, seenToken func(string)) http.Handler {
	stopped := &stoppedSessions{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		raw, readErr := io.ReadAll(r.Body)
		var call agytest.ModelCall
		account := currentPool().Account(name)
		if "/"+path != agytest.ModelCallPath || account == nil || readErr != nil || json.Unmarshal(raw, &call) != nil {
			t.Errorf("unexpected Cloud Code request %s", r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if stopped.has(call.Session) {
			http.Error(w, "stopped by risk control", http.StatusForbidden)
			return
		}
		seenToken(currentGateway().CallTokenOf(name, call.Session))
		var failure *pool.ModelFailure
		if call.Code != http.StatusOK || call.Truncated {
			if call.Code == http.StatusForbidden && strings.Contains(call.Message, "VALIDATION_REQUIRED") {
				account.SetVerification(true, "https://accounts.google.com/v")
			}
			if call.Code == http.StatusBadRequest && isLocationUnsupported(call.Message) {
				account.PauseWithReason(call.Message)
			}
			if call.Code == http.StatusForbidden && isAccountBanned(call.Message) {
				account.PauseWithReason(call.Message)
			}
			failure = &pool.ModelFailure{Code: call.Code, Message: call.Message, Truncated: call.Truncated}
			if call.RetryAfter > 0 {
				failure.Reset = time.Now().Add(call.RetryAfter)
			}
		}
		switch account.RecordModelCall(call.Session, call.Model, failure) {
		case pool.CallRisky:
			stopped.add(call.Session)
			currentGateway().StopRiskySession(account, call.Session, raw, []byte(call.Message))
		case pool.CallStuck:
			stopped.add(call.Session)
			currentGateway().StopStuckSession(account, call.Session, *failure)
		}
	})
}

// stoppedSessions is the sessions the stand-in Cloud Code proxy refuses
// calls of, as cloudcode does once the account stops a session.
type stoppedSessions struct {
	mu       sync.Mutex
	sessions map[string]bool
}

func (s *stoppedSessions) add(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]bool{}
	}
	s.sessions[session] = true
}

func (s *stoppedSessions) has(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[session]
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
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
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

// waitRequests waits until n requests with the method are logged, and
// returns them; fewer after a few seconds.
func (h *harness) waitRequests(method string, n int) []acpRequest {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.requests(method)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var readTool = conv.ToolSpec{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}

func user(text string) conv.Message { return conv.Message{Role: conv.RoleUser, Text: text} }

// A model call names the request its turn serves now: each request gets
// its own token, and the Record carries it.
func TestModelCallsCarryTheirRequestsToken(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	// "@ok" makes the fake agy report one successful model call.
	messages := []conv.Message{user("@ok")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("@ok"))
	if second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); second.err != nil {
		t.Fatalf("second = %+v", second)
	}

	records := h.records.all(t, 2)
	h.tokens.mu.Lock()
	seen := slices.Clone(h.tokens.list)
	h.tokens.mu.Unlock()
	if len(records) != 2 || records[0].CallToken == "" || records[0].CallToken == records[1].CallToken {
		t.Fatalf("record tokens = %+v", records)
	}
	if want := []string{records[0].CallToken, records[1].CallToken}; !slices.Equal(seen, want) {
		t.Fatalf("model calls carried %v, want %v", seen, want)
	}
	if token := h.gw.CallTokenOf("acct0", "no-such-session"); token != "" {
		t.Fatalf("token of an unknown session = %q", token)
	}
}

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

// The janitor kills an account's agy process once it has held no session
// for the idle-close period; the next request starts a new one.
func TestIdleProcIsRetired(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	account := h.p.Accounts()[0]
	const idle = 50 * time.Millisecond

	messages := []conv.Message{user("hello")}
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}
	front := account.LiveProcs()[0]

	// An open session keeps its process, however long it sat idle.
	h.gw.SetIdleClose(idle)
	h.gw.keepForQuota = func(context.Context, *pool.Account, time.Time) bool { return true }
	time.Sleep(2 * idle)
	h.gw.sweep()
	if len(h.gw.Sessions()) != 1 || len(account.LiveProcs()) != 1 {
		t.Fatalf("with an open session: sessions %+v, procs %d", h.gw.Sessions(), len(account.LiveProcs()))
	}

	// Closing the last session starts the process's own idle period.
	h.gw.keepForQuota = nil
	h.gw.sweep()
	if len(h.gw.Sessions()) != 0 || len(account.LiveProcs()) != 1 {
		t.Fatalf("right after the last close: sessions %+v, procs %d", h.gw.Sessions(), len(account.LiveProcs()))
	}

	time.Sleep(2 * idle)
	h.gw.sweep()
	if n := len(account.LiveProcs()); n != 0 {
		t.Fatalf("idle process kept: %d procs", n)
	}
	select {
	case <-front.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("retired process still running")
	}

	// The conversation goes on in a new process.
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if second.err != nil || second.text != "reply to: again" {
		t.Fatalf("after retiring the process = %+v", second)
	}
	if procs := account.LiveProcs(); len(procs) != 1 || procs[0] == front {
		t.Fatalf("procs after the next request = %v, old %p", procs, front)
	}
}

// A session/close that agy never answers still frees the process: the
// gateway forgets the session either way, and a process that kept it as
// open would never count as idle.
func TestUnansweredCloseDoesNotPinTheProc(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_IGNORE_CLOSE=1"}})
	ctx := context.Background()
	account := h.p.Accounts()[0]
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}

	closeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := h.gw.CloseSession(closeCtx, "acct0", h.gw.Sessions()[0].ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close = %v, want a deadline error", err)
	}
	if sessions := h.gw.Sessions(); len(sessions) != 0 {
		t.Fatalf("sessions after the failed close = %+v", sessions)
	}

	const idle = 50 * time.Millisecond
	h.gw.SetIdleClose(idle)
	time.Sleep(2 * idle)
	h.gw.sweep()
	if n := len(account.LiveProcs()); n != 0 {
		t.Fatalf("process kept for a session the gateway forgot: %d procs", n)
	}
}

// Opening a session takes agy a worker start. A client that leaves
// meanwhile never learns the id agy gives, so the session is closed when
// agy answers.
const (
	slowOpen  = "FAKE_AGY_OPEN_DELAY=400ms"
	impatient = 100 * time.Millisecond
)

func TestAbandonedNewSessionIsClosed(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {slowOpen}})
	ctx, cancel := context.WithTimeout(context.Background(), impatient)
	defer cancel()
	if res := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}}); res.text != "" {
		t.Fatalf("result = %+v, want the client gone before any reply", res)
	}

	closes := h.waitRequests("session/close", 1)
	if len(h.requests("session/new")) != 1 || len(closes) != 1 || !strings.HasPrefix(closes[0].Params.SessionID, "fake-") {
		t.Fatalf("new %d, closes %+v", len(h.requests("session/new")), closes)
	}
	if sessions := h.gw.Sessions(); len(sessions) != 0 {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestAbandonedReopenIsClosed(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {slowOpen}})
	bg := context.Background()
	messages := []conv.Message{user("hello")}
	first := h.complete(bg, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}
	id := h.gw.Sessions()[0].ID
	if err := h.gw.CloseSession(bg, "acct0", id); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(bg, impatient)
	defer cancel()
	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	if second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); second.text != "" {
		t.Fatalf("second = %+v, want the client gone before any reply", second)
	}

	closes := h.waitRequests("session/close", 2)
	if len(h.requests("session/resume")) != 1 || len(closes) != 2 || closes[1].Params.SessionID != id {
		t.Fatalf("resumes %d, closes %+v, want the second close for %s", len(h.requests("session/resume")), closes, id)
	}
}

// A retry reopens the session while the abandoned reopen is still pending;
// the abandoned one answers first, and must not close what the retry opens.
func TestAbandonedReopenDoesNotCloseALaterReopen(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {slowOpen}})
	bg := context.Background()
	messages := []conv.Message{user("hello")}
	first := h.complete(bg, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if first.err != nil {
		t.Fatalf("first = %+v", first)
	}
	if err := h.gw.CloseSession(bg, "acct0", h.gw.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}

	messages = append(messages, conv.Message{Role: conv.RoleAssistant, Text: first.text}, user("again"))
	ctx, cancel := context.WithTimeout(bg, impatient)
	defer cancel()
	if abandoned := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages}); abandoned.text != "" {
		t.Fatalf("abandoned = %+v, want the client gone before any reply", abandoned)
	}
	retry := h.complete(bg, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages})
	if retry.err != nil || retry.text != "reply to: again" {
		t.Fatalf("retry = %+v", retry)
	}
	if n := len(h.requests("session/close")); n != 1 {
		t.Fatalf("closes = %d, want the one from CloseSession only", n)
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

// A model call that asks for tools one after the other answers its later
// tools with responses that made no model call, so they report no usage:
// a response's usage is its own, which clients add up.
func TestSerialToolCallsReportNoUsageForLaterTools(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"
	messages := []conv.Message{user("@serial read read")}

	var calls []result
	for range 2 {
		call := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
		if call.finish != conv.FinishToolCalls {
			t.Fatalf("response %d finish = %q, want tool_calls", len(calls), call.finish)
		}
		calls = append(calls, call)
		messages = append(messages,
			conv.Message{Role: conv.RoleAssistant, ToolCalls: call.calls},
			conv.Message{Role: conv.RoleTool, ToolCallID: call.calls[0].ID, Text: "x"})
	}
	done := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})

	want := []conv.Usage{oneCall, {}}
	for i, call := range calls {
		if call.usage == nil || *call.usage != want[i] {
			t.Errorf("tool-call response %d usage %+v, want %+v", i, call.usage, want[i])
		}
	}
	if done.usage == nil || *done.usage != oneCall {
		t.Errorf("final response usage %+v, want %+v (the turn's total less the one call reported)", done.usage, oneCall)
	}

	records := h.records.all(t, 3)
	wantRecorded := []conv.Usage{oneCall, {}, oneCall}
	for i, r := range records {
		if r.Usage == nil || *r.Usage != wantRecorded[i] {
			t.Errorf("record %d usage %+v, want %+v", i, r.Usage, wantRecorded[i])
		}
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

func TestRefuseHarnessPrompt(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	harnessDefault := []conv.Message{{Role: conv.RoleSystem, Text: "be brief"},
		{Role: conv.RoleSystem, Text: "You are opencode, an interactive CLI tool that helps users with software engineering tasks."}, user("hello")}
	own := []conv.Message{{Role: conv.RoleSystem, Text: "You are my assistant; opencode runs you."}, user("hello")}

	if res := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: harnessDefault}); res.err != nil {
		t.Fatalf("refusal off: %v", res.err)
	}

	signatures := []sysprompt.Signature{{Name: "OpenCode", Phrases: []string{"You are opencode,"}}}
	h.gw.SetHarnessPrompts(signatures, true)
	refused := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: harnessDefault})
	if refused.err == nil || refused.err.Kind != conv.ErrForbidden || !strings.Contains(refused.err.Message, "OpenCode") {
		t.Fatalf("err = %+v, want a refusal naming OpenCode", refused.err)
	}
	if want := "\n\nmatched text (marked with >> <<):\n\n>>You are opencode,<< an interactive CLI tool"; !strings.Contains(refused.err.Message, want) || strings.Contains(refused.err.Message, "be brief") {
		t.Fatalf("message = %q, want the matched paragraph %q alone", refused.err.Message, want)
	}
	if res := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: own}); res.err != nil {
		t.Fatalf("own prompt: %v", res.err)
	}
	if n := len(h.requests("session/prompt")); n != 2 {
		t.Fatalf("session/prompt count = %d, want 2 (the refused request reaches no agy)", n)
	}
	records := h.records.all(t, 3)
	if len(records) != 3 || records[1].Err == nil || records[1].Err.Kind != conv.ErrForbidden {
		t.Fatalf("records = %+v, want the refusal recorded second", records)
	}

	h.gw.SetHarnessPrompts(signatures, false)
	if res := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: harnessDefault}); res.err != nil {
		t.Fatalf("refusal turned off again: %v", res.err)
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
	if got := opened[0].Params.Meta.Antigravity.SystemPrompt.Append; !strings.HasSuffix(got, "\n"+clientPromptOpen+"\nbe brief\n\nbe kind\n"+clientPromptClose) {
		t.Fatalf("append = %q, want the leading system messages in the client tag at its end", got)
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
	if len(opened) != 2 || !strings.HasSuffix(opened[1].Params.Meta.Antigravity.SystemPrompt.Append, "\n"+clientPromptOpen+"\nbe terse\n\nbe kind\n"+clientPromptClose) {
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

// A replayed conversation keeps the inline media of its history, numbered in
// the text and attached ahead of the newest input's own.
func TestFreshPromptCarriesHistoryImages(t *testing.T) {
	messages := []conv.Message{
		{Role: conv.RoleUser, Text: "first", Media: []conv.Media{{Data: "AAAA", MimeType: "image/png"}}},
		{Role: conv.RoleAssistant, ToolCalls: []conv.ToolCall{{ID: "c1", Name: "shot", Arguments: json.RawMessage(`{}`)}}},
		{Role: conv.RoleTool, ToolCallID: "c1", ToolName: "shot", Text: "done", Media: []conv.Media{{Data: "BBBB", MimeType: "audio/wav"}}},
		{Role: conv.RoleAssistant, Text: "seen"},
		{Role: conv.RoleUser, Text: "newest", Media: []conv.Media{{Data: "CCCC", MimeType: "application/pdf"}}},
	}
	blocks := freshPrompt(newTranscript(messages))

	var got []string
	for _, block := range blocks[:len(blocks)-1] {
		got = append(got, block.Type+":"+block.MimeType+":"+block.Data)
	}
	want := []string{"image:image/png:AAAA", "image:audio/wav:BBBB", "image:application/pdf:CCCC"}
	if !slices.Equal(got, want) {
		t.Fatalf("blocks before the text = %v, want %v", got, want)
	}
	text := blocks[len(blocks)-1].Text
	for _, want := range []string{
		"first\n[Attached media 1 (image/png)]",
		"done\n[Attached media 2 (audio/wav)]",
		"\n\nnewest",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, historyPlaceholder) {
		t.Errorf("history media still replaced by a placeholder:\n%s", text)
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

func TestToolResultSteersTrailingUserMessage(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	messages := []conv.Message{user(`@tool read {}`)}
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}

	steerImg := conv.Media{MimeType: "image/png", Data: "iVBORw0KGgo="}
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls, Anchors: []string{first.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Text: "contents"},
		conv.Message{Role: conv.RoleUser, Text: "check line 10", Media: []conv.Media{steerImg}},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	wantText := "tool said: contents\nuser steered: [image:image/png] check line 10"
	if second.err != nil || second.finish != conv.FinishStop || second.text != wantText {
		t.Fatalf("second = %+v, want %q", second, wantText)
	}
	if first.usage == nil || *first.usage != oneCall || second.usage == nil || *second.usage != oneCall {
		t.Fatalf("usage = (%+v, %+v), want (%+v, %+v)", first.usage, second.usage, oneCall, oneCall)
	}

	prompts := h.requests("session/prompt")
	if len(prompts) != 2 || prompts[0].Params.SessionID != prompts[1].Params.SessionID {
		t.Fatalf("prompts = %+v, want 2 prompts on the same session", prompts)
	}
	gotBlocks := prompts[1].Params.Prompt
	if len(gotBlocks) != 2 || gotBlocks[0].Type != "image" || gotBlocks[0].MimeType != "image/png" || gotBlocks[0].Data != steerImg.Data || gotBlocks[1].Type != "text" || gotBlocks[1].Text != "check line 10" {
		t.Fatalf("steer prompt blocks = %+v", gotBlocks)
	}

	// Follow-up warm turn sees all 5 steps (USER_INPUT, PLANNER, GENERIC,
	// steered USER_INPUT, final PLANNER), and rewinding back to the post-steer
	// reply keeps those 5 steps.
	afterSteer := append(slices.Clone(messages), conv.Message{Role: conv.RoleAssistant, Text: second.text, Anchors: []string{second.anchor}})
	warm := h.complete(ctx, "alice", conv.Request{Model: model, Messages: append(slices.Clone(afterSteer), user("@steps")), Tools: tools})
	if warm.err != nil || warm.text != "steps: 5" {
		t.Fatalf("warm = %+v, want steps: 5", warm)
	}
	branched := h.complete(ctx, "alice", conv.Request{Model: model, Messages: append(slices.Clone(afterSteer), user("@steps")), Tools: tools})
	if branched.err != nil || branched.text != "steps: 5" {
		t.Fatalf("branched = %+v, want steps: 5", branched)
	}
	h.assertRewound(prompts[0].Params.SessionID, 5)
}

func TestMultimodalPromptAndToolResult(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	userMedia := []conv.Media{
		{MimeType: "audio/wav", Data: "AQIDBA=="},
		{MimeType: "application/pdf", Data: "JVBERi0xLjQK"},
	}
	messages := []conv.Message{{Role: conv.RoleUser, Text: `@tool read {}`, Media: userMedia}}
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}

	toolMedia := []conv.Media{
		{MimeType: "audio/mp3", Data: "AQID"},
		{MimeType: "video/mp4", Data: "AAAAFGZ0eXBpc29t"},
	}
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls, Anchors: []string{first.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Media: toolMedia},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: model, Messages: messages, Tools: tools})
	wantText := "tool said: (media result)\n[media:audio/mp3:3]\n[media:video/mp4:12]"
	if second.err != nil || second.finish != conv.FinishStop || second.text != wantText {
		t.Fatalf("second = %+v, want %q", second, wantText)
	}

	prompts := h.requests("session/prompt")
	if len(prompts) != 1 {
		t.Fatalf("prompts = %+v, want 1 prompt", prompts)
	}
	blocks := prompts[0].Params.Prompt
	if len(blocks) != 3 ||
		blocks[0].Type != "image" || blocks[0].MimeType != "audio/wav" || blocks[0].Data != "AQIDBA==" ||
		blocks[1].Type != "image" || blocks[1].MimeType != "application/pdf" || blocks[1].Data != "JVBERi0xLjQK" {
		t.Fatalf("initial prompt blocks = %+v", blocks)
	}
}

func TestToolResultSplitsSystemAndUserMessages(t *testing.T) {
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
		conv.Message{Role: conv.RoleSystem, Text: "before user"},
		conv.Message{Role: conv.RoleUser, Text: "user question"},
		conv.Message{Role: conv.RoleSystem, Text: "after user"},
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err != nil {
		t.Fatalf("second = %+v", second)
	}
	if !strings.Contains(second.text, "tool said: contents\n\n<client-steering>") || !strings.Contains(second.text, "<system>\nbefore user\n</system>") {
		t.Fatalf("second.text = %q, want pre-user system note on tool result", second.text)
	}
	if !strings.Contains(second.text, "user steered: <user>\nuser question\n</user>\n\n<system>\nafter user\n</system>") {
		t.Fatalf("second.text = %q, want user and trailing system in steer prompt", second.text)
	}
	if n := len(h.requests("session/prompt")); n != 2 {
		t.Fatalf("session/prompt count = %d, want 2", n)
	}
}

func TestSerialAndChainedToolSteers(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	req := func(messages []conv.Message) conv.Request {
		return conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools}
	}

	// 1. Serial tool calls steered on both steps: call 2 reports zero usage,
	// and both user messages arrive before the final model call.
	mSerial := []conv.Message{user("@serial read read")}
	s1 := h.complete(ctx, "alice", req(mSerial))
	mSerial = append(mSerial,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: s1.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: s1.calls[0].ID, Text: "s1"},
		user("steer 1"),
	)
	s2 := h.complete(ctx, "alice", req(mSerial))
	if s2.err != nil || s2.finish != conv.FinishToolCalls || s2.usage == nil || (*s2.usage != conv.Usage{}) {
		t.Fatalf("s2 = %+v, want tool_calls with zero usage", s2)
	}
	mSerial = append(mSerial,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: s2.calls},
		conv.Message{Role: conv.RoleTool, ToolCallID: s2.calls[0].ID, Text: "s2"},
		user("steer 2"),
	)
	s3 := h.complete(ctx, "alice", req(mSerial))
	wantSerial := "tool said: s2\nuser steered: steer 1 | steer 2"
	if s3.err != nil || s3.text != wantSerial || s3.usage == nil || *s3.usage != oneCall {
		t.Fatalf("s3 = %+v, want %q and usage %+v", s3, wantSerial, oneCall)
	}

	// 2. Chained tool calls steered after call 1 record a rewindable position
	// at step 4 (USER_INPUT, PLANNER, GENERIC, steered USER_INPUT), so when
	// call 3's response is retried, it rewinds to step 4 instead of step 1.
	mChain := []conv.Message{user("@toolchain read read read")}
	c1 := h.complete(ctx, "bob", req(mChain))
	mChain = append(mChain,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: c1.calls, Anchors: []string{c1.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: c1.calls[0].ID, Text: "c1"},
		user("chain steer"),
	)
	c2 := h.complete(ctx, "bob", req(mChain))
	if c2.err != nil || c2.finish != conv.FinishToolCalls || c2.usage == nil || *c2.usage != oneCall {
		t.Fatalf("c2 = %+v", c2)
	}
	mChain = append(mChain,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: c2.calls, Anchors: []string{c2.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: c2.calls[0].ID, Text: "c2"},
	)
	c3 := h.complete(ctx, "bob", req(mChain))
	if c3.err != nil || c3.finish != conv.FinishToolCalls {
		t.Fatalf("c3 = %+v", c3)
	}
	retryChain := h.complete(ctx, "bob", req(mChain))
	if retryChain.err != nil {
		t.Fatalf("retryChain = %+v", retryChain)
	}
	prompts := h.requests("session/prompt")
	h.assertRewound(prompts[len(prompts)-1].Params.SessionID, 4)
}

func TestFailedSteerReportsUpstreamError(t *testing.T) {
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
		user("@failsteer"),
	)
	second := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools})
	if second.err == nil || second.err.Kind != conv.ErrUpstream {
		t.Fatalf("second = %+v, want ErrUpstream when steer fails", second)
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

// A quota failure is the account's: the client gets the error once, with a
// short Retry-After, and its retry is served by another account.
func TestQuotaFailureSwitchesAccountOnTheNextRequest(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}

	first := h.complete(ctx, name, req)
	if first.err == nil || first.err.Kind != conv.ErrQuota || first.err.HTTPStatus != 0 || first.err.RetryAfterSeconds != switchRetryAfterSeconds ||
		!strings.Contains(first.err.Message, agytest.QuotaError) || !strings.Contains(first.err.Message, "next request switches to another account") {
		t.Fatalf("first = %+v, want the quota error, a %ds Retry-After, and the switch notice", first.err, switchRetryAfterSeconds)
	}
	if err := h.p.Account("acct0").Available("gemini"); err == nil {
		t.Fatal("acct0 is not cooling down")
	}

	second := h.complete(ctx, name, req)
	if second.err != nil || second.text != "reply to: hi" {
		t.Fatalf("retry = %+v", second)
	}
	if records := h.records.all(t, 2); records[0].Account != "acct0" || records[1].Account != "acct1" {
		t.Fatalf("accounts = %s, %s; want acct0 then acct1", records[0].Account, records[1].Account)
	}
}

// A 429 that names a short wait is passed on as Google sent it: no switch
// notice, no count, and the conversation stays on its account.
func TestShortQuotaWaitIsPassedThrough(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_QUOTA=1", "FAKE_AGY_QUOTA_WAIT=15s"}})
	name := userPreferring(t, h.p, "acct0")
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}

	limited := h.complete(context.Background(), name, req)
	if limited.err == nil || limited.err.Kind != conv.ErrQuota || limited.err.HTTPStatus != 0 || limited.err.RetryAfterSeconds != 15 || limited.err.Message != agytest.QuotaError {
		t.Fatalf("limited = %+v, want Google's 429 with its 15 s and no notice", limited.err)
	}
	keys := h.gw.conversationKeys(testCaller(name).KeyHash, req.Model+"\x00", newTranscript(req.Messages))
	if _, ok := h.gw.conversations.get(keys); ok {
		t.Fatal("a passed-through 429 left a failure state")
	}
}

// An account that only cools down for a short time is waited for, not left:
// the conversation holding a session there gets the 429 with the time left.
func TestShortCooldownOnTheConversationsAccountIsAWait(t *testing.T) {
	h := newHarness(t, 2, nil)
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	first := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two")}}

	h.p.Account("acct0").Cool("gemini", time.Now().Add(15*time.Second))
	waiting := h.complete(ctx, name, req)
	if waiting.err == nil || waiting.err.Kind != conv.ErrQuota || waiting.err.RetryAfterSeconds <= 0 || waiting.err.RetryAfterSeconds > 15 ||
		strings.Contains(waiting.err.Message, "[agyrelay]") {
		t.Fatalf("while cooling = %+v, want a plain 429 with the time left", waiting.err)
	}

	// A cooldown no client waits out moves the conversation: told once,
	// then served by the other account.
	h.p.Account("acct0").Cool("gemini", time.Now().Add(2*time.Hour))
	long := h.complete(ctx, name, req)
	if long.err == nil || long.err.RetryAfterSeconds != switchRetryAfterSeconds || !strings.Contains(long.err.Message, "account holding this conversation is unavailable") {
		t.Fatalf("long cooldown = %+v, want the account-unavailable error with a %d s Retry-After", long.err, switchRetryAfterSeconds)
	}
	if moved := h.complete(ctx, name, req); moved.err != nil {
		t.Fatalf("after the switch notice = %+v", moved.err)
	}
	if rec := h.records.all(t, 4)[3]; rec.Account != "acct1" || rec.ReplayReason != ReplaySwitchDefinite {
		t.Fatalf("moved request = %s %s, want acct1 %s", rec.Account, rec.ReplayReason, ReplaySwitchDefinite)
	}
}

// A failure that may pass counts down the retries left on the account;
// the request after the last one switches accounts, and the conversation's
// history replays there.
func TestRepeatedFailuresSwitchAccountAfterThreeRetries(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_FAIL_CODE=503", "FAKE_AGY_FAIL_MSG=UNAVAILABLE (code 503): backend down"}})
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}

	for failures := 1; failures <= failuresBeforeSwitch; failures++ {
		r := h.complete(ctx, name, req)
		want := fmt.Sprintf("Retries left before switching accounts: %d", failuresBeforeSwitch-failures)
		if failures == failuresBeforeSwitch {
			want = fmt.Sprintf("(%d of %d). %s", failures, failuresBeforeSwitch, switchWarning)
		}
		if r.err == nil || r.err.Kind != conv.ErrOverloaded || !strings.Contains(r.err.Message, "backend down") || !strings.Contains(r.err.Message, want) {
			t.Fatalf("failure %d = %+v, want a 503 saying %q", failures, r.err, want)
		}
	}
	if r := h.complete(ctx, name, req); r.err != nil || r.text != "reply to: hi" {
		t.Fatalf("request after the retries = %+v", r)
	}
	records := h.records.all(t, failuresBeforeSwitch+1)
	for i, rec := range records[:failuresBeforeSwitch] {
		if rec.Account != "acct0" {
			t.Fatalf("failure %d ran on %s, want acct0: the conversation stays until its retries are used", i+1, rec.Account)
		}
	}
	if last := records[failuresBeforeSwitch]; last.Account != "acct1" {
		t.Fatalf("switched request ran on %s, want acct1", last.Account)
	}
}

// A retry of a failed model call stays on the account and rewinds past
// the failed input, so the session keeps the conversation's reasoning.
func TestRetryAfterModelFailureRewindsTheSession(t *testing.T) {
	h := newHarness(t, 2, nil)
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	first := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two @failonce")}}

	failed := h.complete(ctx, name, req)
	if failed.err == nil || !strings.Contains(failed.err.Message, agytest.FailOnceError) || !strings.Contains(failed.err.Message, "Retries left before switching accounts: 2") {
		t.Fatalf("failed = %+v", failed.err)
	}
	retried := h.complete(ctx, name, req)
	if retried.err != nil || retried.text != "reply to: two @failonce" {
		t.Fatalf("retry = %+v", retried)
	}
	records := h.records.all(t, 3)
	if got := records[2]; got.Account != records[0].Account || got.Mode != ModeRewind {
		t.Fatalf("retry = %s %s, want %s rewind", got.Account, got.Mode, records[0].Account)
	}
}

// A success ends the run of failures: the next failure counts from one.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	h := newHarness(t, 2, nil)
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	left := func(r result, n int) bool {
		return r.err != nil && strings.Contains(r.err.Message, fmt.Sprintf("Retries left before switching accounts: %d", n))
	}
	firstReq := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one @failonce")}}
	if r := h.complete(ctx, name, firstReq); !left(r, failuresBeforeSwitch-1) {
		t.Fatalf("first failure = %+v", r.err)
	}
	ok := h.complete(ctx, name, firstReq)
	if ok.err != nil {
		t.Fatalf("retry = %+v", ok.err)
	}
	secondReq := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one @failonce"), {Role: conv.RoleAssistant, Text: ok.text}, user("two @failonce")}}
	if r := h.complete(ctx, name, secondReq); !left(r, failuresBeforeSwitch-1) {
		t.Fatalf("failure after a success = %+v, want the count to start again", r.err)
	}
}

// The error of a model the conversation's account lacks is about that
// model: going back to the model, the client continues on the account
// with the session and its reasoning, as if nothing happened.
func TestChangingBackAfterAModelErrorKeepsTheAccount(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{
		0: {"FAKE_AGY_MODELS=gemini-3.8-flash-low,gemini-3.8-flash-high"},
		1: {"FAKE_AGY_MODELS=gemini-3.8-flash-medium,claude-opus-5-5-high"},
	})
	ctx := context.Background()
	alice := userPreferring(t, h.p, "acct0")
	request := func(model string, messages ...conv.Message) result {
		return h.complete(ctx, alice, conv.Request{Model: model, Messages: messages})
	}
	// Both accounts report their catalogs.
	if r := request("claude-opus-5-5", user("c")); r.err != nil {
		t.Fatalf("claude = %+v", r.err)
	}
	first := request("gemini-3.8-flash", user("one"))
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	messages := []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two")}

	if r := request("claude-opus-5-5", messages...); r.err == nil {
		t.Fatal("claude on the conversation's account, which lacks it, did not fail")
	}
	if r := request("gemini-3.8-flash", messages...); r.err != nil {
		t.Fatalf("back to gemini = %+v", r.err)
	}
	records := h.records.all(t, 4)
	if got := records[3]; got.Account != "acct0" || got.Mode == ModeReplay {
		t.Fatalf("back to gemini ran on %s as %s, want acct0 continuing its session", got.Account, got.Mode)
	}
}

// Leaving an account is allowed, not forced: when no other account can
// serve the request, a conversation that is due to switch stays where its
// session is.
func TestDueConversationStaysWhenNoOtherAccountCanServe(t *testing.T) {
	h := newHarness(t, 2, nil)
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	first := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	messages := []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two")}
	req := conv.Request{Model: "gemini-3.8-flash", Messages: messages}

	// The conversation used up its retries on acct0.
	r := &run{req: req, model: modelChoice{id: "gemini-3.8-flash"}}
	keys := h.gw.conversationKeys(testCaller(name).KeyHash, r.choice(), newTranscript(messages))
	h.gw.conversations.put(keys, conversationState{account: h.p.Account("acct0").ID(), failures: failuresBeforeSwitch})

	h.p.Account("acct1").SetPaused(true)
	if got := h.complete(ctx, name, req); got.err != nil {
		t.Fatalf("with no other account = %+v, want to stay on acct0", got.err)
	}
	if rec := h.records.all(t, 2)[1]; rec.Account != "acct0" || rec.Mode == ModeReplay {
		t.Fatalf("stayed request = %s %s, want acct0 continuing its session", rec.Account, rec.Mode)
	}

	// Once another account can serve, the next request leaves.
	h.gw.conversations.put(keys, conversationState{account: h.p.Account("acct0").ID(), failures: failuresBeforeSwitch})
	h.p.Account("acct1").SetPaused(false)
	if got := h.complete(ctx, name, req); got.err != nil {
		t.Fatalf("with acct1 back = %+v", got.err)
	}
	if rec := h.records.all(t, 3)[2]; rec.Account != "acct1" || rec.ReplayReason != ReplaySwitchFailures {
		t.Fatalf("switched request = %s %s, want acct1 %s", rec.Account, rec.ReplayReason, ReplaySwitchFailures)
	}
}

// With history, the request that switches accounts says why it replays.
func TestSwitchedRequestNamesItsReplayReason(t *testing.T) {
	h := newHarness(t, 2, nil)
	name := userPreferring(t, h.p, "acct0")
	ctx := context.Background()
	first := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	h.p.Account("acct0").SetPaused(true)
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")}}

	refused := h.complete(ctx, name, req)
	if refused.err == nil || !strings.Contains(refused.err.Message, "account holding this conversation is unavailable") {
		t.Fatalf("paused account = %+v, want an error naming why the session cannot be resumed", refused.err)
	}
	if r := h.complete(ctx, name, req); r.err != nil {
		t.Fatalf("retry = %+v", r.err)
	}
	got := h.records.all(t, 3)[2]
	if got.Account != "acct1" || got.Mode != ModeReplay || got.ReplayReason != ReplaySwitchDefinite {
		t.Fatalf("retry = %s %s %s, want acct1 replay %s", got.Account, got.Mode, got.ReplayReason, ReplaySwitchDefinite)
	}
}

// Pausing an account (manually or automatically) must stop continuing turns
// parked on it, release the parked worker slot, and route through the standard
// account-unavailable switch path even under MaxWorkers = 1.
func TestPausedAccountRefusesParkedTurnContinueAndSwitches(t *testing.T) {
	cases := []struct {
		name  string
		pause func(a *pool.Account)
	}{
		{"manual", func(a *pool.Account) { a.SetPaused(true) }},
		{"auto", func(a *pool.Account) { a.PauseWithReason("TOS_VIOLATION") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 2, nil)
			ctx := context.Background()
			tools := []conv.ToolSpec{readTool}
			one := 1
			alice := testCaller("alice")
			alice.MaxWorkers = &one

			first := h.completeAs(ctx, alice, conv.Request{
				Model:    "gemini-3.8-flash",
				Tools:    tools,
				Messages: []conv.Message{user(`@tool read {"path":"a.txt"}`)},
			})
			if first.err != nil || first.finish != conv.FinishToolCalls || len(first.calls) != 1 {
				t.Fatalf("first = %+v", first)
			}
			firstRec := h.records.all(t, 1)[0]
			pausedID := firstRec.Account
			otherID := "acct1"
			if pausedID == "acct1" {
				otherID = "acct0"
			}
			if !h.turnIs(TurnParked) {
				t.Fatalf("sessions = %+v, want parked turn on %s", h.gw.Sessions(), pausedID)
			}

			tc.pause(h.p.Account(pausedID))

			call := first.calls[0]
			contReq := conv.Request{
				Model: "gemini-3.8-flash",
				Tools: tools,
				Messages: []conv.Message{
					user(`@tool read {"path":"a.txt"}`),
					{Role: conv.RoleAssistant, ToolCalls: first.calls},
					{Role: conv.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Text: "contents of a"},
				},
			}
			refused := h.completeAs(ctx, alice, contReq)
			if refused.err == nil || refused.err.Kind != conv.ErrOverloaded || refused.err.RetryAfterSeconds != switchRetryAfterSeconds ||
				!strings.Contains(refused.err.Message, "account holding this conversation is unavailable") ||
				!strings.Contains(refused.err.Message, switchWarning) {
				t.Fatalf("continue on paused account = %+v, want 503 switch notice", refused.err)
			}
			if h.p.Account(pausedID).Turns() != 0 {
				t.Fatalf("paused account still has %d active turns after definite rejection", h.p.Account(pausedID).Turns())
			}

			switched := h.completeAs(ctx, alice, contReq)
			if switched.err != nil || switched.finish != conv.FinishStop {
				t.Fatalf("retry after pause = %+v", switched)
			}
			recs := h.records.all(t, 3)
			if recs[2].Account != otherID || recs[2].Mode != ModeReplay || recs[2].ReplayReason != ReplaySwitchDefinite {
				t.Fatalf("retry = %s %s %s, want %s replay %s", recs[2].Account, recs[2].Mode, recs[2].ReplayReason, otherID, ReplaySwitchDefinite)
			}
			// Only the initial turn ran on the paused account; the replay ran on otherID.
			prompts := h.requests("session/prompt")
			if len(prompts) != 2 {
				t.Fatalf("session/prompt count = %d, want 2", len(prompts))
			}
			if prompts[0].Params.SessionID == prompts[1].Params.SessionID {
				t.Fatal("retry reused the paused account's session")
			}
		})
	}
}

// A short cooldown on a parked turn's account returns a plain 429 and leaves
// the parked turn intact to continue once it ends; a long cooldown cancels the
// parked turn and switches accounts on retry.
func TestCooldownOnParkedTurnWaitsWhenShortAndSwitchesWhenLong(t *testing.T) {
	h := newHarness(t, 2, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}

	first := h.complete(ctx, "alice", conv.Request{
		Model:    "gemini-3.8-flash",
		Tools:    tools,
		Messages: []conv.Message{user(`@tool read {"path":"a.txt"}`)},
	})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}
	firstAcct := h.records.all(t, 1)[0].Account
	call := first.calls[0]
	contReq := conv.Request{
		Model: "gemini-3.8-flash",
		Tools: tools,
		Messages: []conv.Message{
			user(`@tool read {"path":"a.txt"}`),
			{Role: conv.RoleAssistant, ToolCalls: first.calls},
			{Role: conv.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Text: "ok"},
		},
	}

	h.p.Account(firstAcct).Cool(pool.GroupGemini, time.Now().Add(150*time.Millisecond))
	waiting := h.complete(ctx, "alice", contReq)
	if waiting.err == nil || waiting.err.Kind != conv.ErrQuota || waiting.err.RetryAfterSeconds <= 0 ||
		strings.Contains(waiting.err.Message, "[agyrelay]") {
		t.Fatalf("short cooldown on parked turn = %+v, want plain 429", waiting.err)
	}
	if !h.turnIs(TurnParked) {
		t.Fatalf("short cooldown cancelled parked turn: sessions = %+v", h.gw.Sessions())
	}

	time.Sleep(200 * time.Millisecond)
	continued := h.complete(ctx, "alice", contReq)
	if continued.err != nil || continued.text != "tool said: ok" {
		t.Fatalf("after short cooldown = %+v", continued)
	}
	if recs := h.records.all(t, 3); recs[2].Account != firstAcct || recs[2].Mode != ModeContinue {
		t.Fatalf("after short cooldown = %s %s, want %s continue", recs[2].Account, recs[2].Mode, firstAcct)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("session/prompt count = %d, want 1 (same parked turn)", n)
	}

	// Now park a second turn and apply a long cooldown: definite error + switch.
	second := h.complete(ctx, "bob", conv.Request{
		Model:    "gemini-3.8-flash",
		Tools:    tools,
		Messages: []conv.Message{user(`@tool read {"path":"b.txt"}`)},
	})
	if second.err != nil || len(second.calls) != 1 {
		t.Fatalf("second = %+v", second)
	}
	bobAcct := h.records.all(t, 4)[3].Account
	bobOther := "acct1"
	if bobAcct == "acct1" {
		bobOther = "acct0"
	}
	bobCall := second.calls[0]
	bobCont := conv.Request{
		Model: "gemini-3.8-flash",
		Tools: tools,
		Messages: []conv.Message{
			user(`@tool read {"path":"b.txt"}`),
			{Role: conv.RoleAssistant, ToolCalls: second.calls},
			{Role: conv.RoleTool, ToolCallID: bobCall.ID, ToolName: bobCall.Name, Text: "ok2"},
		},
	}
	h.p.Account(bobAcct).Cool(pool.GroupGemini, time.Now().Add(2*time.Hour))
	long := h.complete(ctx, "bob", bobCont)
	if long.err == nil || long.err.RetryAfterSeconds != switchRetryAfterSeconds ||
		!strings.Contains(long.err.Message, "account holding this conversation is unavailable") {
		t.Fatalf("long cooldown on parked turn = %+v, want switch notice", long.err)
	}
	if h.p.Account(bobAcct).Turns() != 0 {
		t.Fatalf("long cooldown left %d active turns on %s", h.p.Account(bobAcct).Turns(), bobAcct)
	}
	moved := h.complete(ctx, "bob", bobCont)
	if moved.err != nil {
		t.Fatalf("retry after long cooldown = %+v", moved.err)
	}
	if rec := h.records.all(t, 6)[5]; rec.Account != bobOther || rec.Mode != ModeReplay || rec.ReplayReason != ReplaySwitchDefinite {
		t.Fatalf("moved = %s %s %s, want %s replay %s", rec.Account, rec.Mode, rec.ReplayReason, bobOther, ReplaySwitchDefinite)
	}
}

// Pausing an account must leave an already-running turn undisturbed while
// rejecting new requests on that session and switching them on retry.
func TestPausedAccountLeavesRunningTurnAlone(t *testing.T) {
	h := newHarness(t, 2, nil)
	ctx := context.Background()
	name := userPreferring(t, h.p, "acct0")

	first := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("one")}})
	if first.err != nil {
		t.Fatalf("first = %+v", first.err)
	}
	base := []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}}

	// Start a running turn on acct0's session and wait until it is in flight.
	runningDone := make(chan result, 1)
	go func() {
		runningDone <- h.complete(ctx, name, conv.Request{
			Model:    "gemini-3.8-flash",
			Messages: append(slices.Clone(base), user("@hang")),
		})
	}()
	defer func() {
		h.cancelTurns()
		<-runningDone
	}()
	h.waitRequests("session/prompt", 2)
	acct0Running := func() bool {
		for _, s := range h.gw.Sessions() {
			if s.Account == "acct0" && s.Turn != nil && s.Turn.State == TurnRunning {
				return true
			}
		}
		return false
	}
	if !acct0Running() {
		t.Fatalf("sessions = %+v, want running turn on acct0", h.gw.Sessions())
	}

	h.p.Account("acct0").SetPaused(true)

	// A new request matching acct0's earlier snapshot is rejected without
	// cancelling the running turn, and its retry switches to acct1.
	nextReq := conv.Request{Model: "gemini-3.8-flash", Messages: append(slices.Clone(base), user("follow-up"))}
	refused := h.complete(ctx, name, nextReq)
	if refused.err == nil || refused.err.Kind != conv.ErrOverloaded ||
		!strings.Contains(refused.err.Message, "account holding this conversation is unavailable") {
		t.Fatalf("request while paused = %+v, want 503 unavailable", refused.err)
	}
	if !acct0Running() {
		t.Fatalf("running turn was disturbed by pause rejection: sessions = %+v", h.gw.Sessions())
	}

	switched := h.complete(ctx, name, nextReq)
	if switched.err != nil {
		t.Fatalf("retry = %+v", switched.err)
	}
	if !acct0Running() {
		t.Fatalf("running turn on paused account stopped unexpectedly: sessions = %+v", h.gw.Sessions())
	}
	if rec := h.records.all(t, 3)[2]; rec.Account != "acct1" || rec.Mode != ModeReplay || rec.ReplayReason != ReplaySwitchDefinite {
		t.Fatalf("switched = %s %s %s, want acct1 replay %s", rec.Account, rec.Mode, rec.ReplayReason, ReplaySwitchDefinite)
	}
}

// If an account is paused while startFresh is opening a new session on it,
// the request must not send a prompt there and must move to another account.
func TestStartFreshSkipsAccountPausedDuringOpen(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_OPEN_DELAY=150ms"}})
	ctx := context.Background()
	name := userPreferring(t, h.p, "acct0")

	go func() {
		time.Sleep(40 * time.Millisecond)
		h.p.Account("acct0").SetPaused(true)
	}()

	got := h.complete(ctx, name, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if got.err != nil || got.text != "reply to: hi" {
		t.Fatalf("got = %+v", got)
	}
	if rec := h.records.all(t, 1)[0]; rec.Account != "acct1" || rec.Mode != ModeFresh {
		t.Fatalf("record = %s %s, want acct1 fresh", rec.Account, rec.Mode)
	}
	if h.p.Account("acct0").Turns() != 0 {
		t.Fatalf("acct0 leaked %d active turns", h.p.Account("acct0").Turns())
	}
	if prompts := h.requests("session/prompt"); len(prompts) != 1 {
		t.Fatalf("session/prompt count = %d, want 1 (none on paused acct0)", len(prompts))
	}
}

// userPreferring finds a user whose first-choice account is account for the
// opening prompts used by the routing tests.
func userPreferring(t *testing.T, p *pool.Pool, account string) string {
	t.Helper()
	prompts := []string{"hi", "one", "one @failonce", "c", "hello", "c0"}
Search:
	for i := range 2000 {
		candidate := fmt.Sprintf("user%d", i)
		keyHash := testCaller(candidate).KeyHash
		for _, prompt := range prompts {
			tr := newTranscript([]conv.Message{user(prompt)})
			picked, err := p.Pick(sessionAffinity(keyHash, tr), "gemini", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			picked.Release()
			if picked.Name() != account {
				continue Search
			}
		}
		return candidate
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

	// The conversation lives on acct0, which lacks claude: the client is
	// told, and its retry replays the conversation on acct1.
	messages := []conv.Message{user("one"), {Role: conv.RoleAssistant, Text: first.text}, user("two")}
	if r := request("claude-opus-5-5", messages...); r.err == nil || !strings.Contains(r.err.Message, `does not offer model "claude-opus-5-5"`) {
		t.Fatalf("model the conversation's account lacks = %+v, want an error", r.err)
	}
	if r := request("claude-opus-5-5", messages...); r.err != nil {
		t.Fatalf("switch = %+v", r.err)
	}
	records := h.records.all(t, 5)
	want := []struct{ account, slug string }{{"acct1", "claude-opus-5-5-high"}, {"acct0", "gemini-3.8-flash-low"}, {"acct1", "gemini-3.8-flash-medium"}, {"", ""}, {"acct1", "claude-opus-5-5-high"}}
	for i, w := range want {
		if records[i].Account != w.account || records[i].Slug != w.slug {
			t.Errorf("record %d = %s %s, want %s %s", i, records[i].Account, records[i].Slug, w.account, w.slug)
		}
	}
	if records[4].Mode != ModeReplay || records[4].ReplayReason != ReplaySwitchDefinite {
		t.Errorf("switch = %q %q, want replay %s", records[4].Mode, records[4].ReplayReason, ReplaySwitchDefinite)
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

func TestGemini4NeonModel(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{
		0: {"FAKE_AGY_MODELS=gemini-4-neon-low,gemini-4-neon-medium,gemini-4-neon-high,gemini-4-neon-xhigh"},
	})
	var fallback *conv.ModelInfo
	for _, m := range h.gw.Models() {
		if m.ID == "gemini-4-neon" {
			fallback = &m
			break
		}
	}
	if fallback == nil || fallback.Name != "Gemini 4 Neon" ||
		!slices.Equal(fallback.Efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("fallback gemini-4-neon = %+v", fallback)
	}

	ctx := context.Background()
	if r := h.complete(ctx, "alice", conv.Request{Model: "gemini-4-neon", Effort: "xhigh", Messages: []conv.Message{user("hi")}}); r.err != nil {
		t.Fatalf("complete xhigh = %+v", r.err)
	}
	if r := h.complete(ctx, "alice", conv.Request{Model: "gemini-4-neon", Messages: []conv.Message{user("default")}}); r.err != nil {
		t.Fatalf("complete default = %+v", r.err)
	}
	records := h.records.all(t, 2)
	if records[0].Slug != "gemini-4-neon-xhigh" || records[0].Effort != "xhigh" ||
		records[0].QuotaGroup != pool.GroupGemini {
		t.Fatalf("xhigh record = %+v", records[0])
	}
	if records[1].Slug != "gemini-4-neon-medium" || records[1].Effort != "medium" ||
		records[1].QuotaGroup != pool.GroupGemini {
		t.Fatalf("default record = %+v", records[1])
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

func TestQuotaFailureWithoutFallback(t *testing.T) {
	// Long cooldown (2h) with no fallback: must not return retryable 429 or
	// hours-long Retry-After; pass upstream message as non-retryable 403.
	h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrQuota || r.err.HTTPStatus != http.StatusForbidden ||
		r.err.Message != agytest.QuotaError || r.err.RetryAfterSeconds != 0 || r.text != "" {
		t.Fatalf("result = %+v, want non-retryable 403 quota error", r.err)
	}
	// While every account offering the model is in long cooldown, later
	// requests get a non-retryable ErrUnknownModel instead of 503.
	again := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if again.err == nil || again.err.Kind != conv.ErrUnknownModel ||
		strings.Contains(again.err.Message, "acct0") || strings.Contains(again.err.Message, "cooling down") {
		t.Fatalf("cooling down = %+v, want ErrUnknownModel without pool details", again)
	}

	// Short cooldown (15s) keeps retryable 429 with Retry-After, both on
	// the failing call and while the cooldown lasts.
	short := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_QUOTA=1", "FAKE_AGY_QUOTA_WAIT=15s"}})
	r1 := short.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r1.err == nil || r1.err.Kind != conv.ErrQuota || r1.err.HTTPStatus != 0 || r1.err.RetryAfterSeconds != 15 || r1.err.Message != agytest.QuotaError {
		t.Fatalf("short quota = %+v, want 429 with 15s Retry-After", r1.err)
	}
	r2 := short.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r2.err == nil || r2.err.Kind != conv.ErrQuota || r2.err.HTTPStatus != 0 || r2.err.RetryAfterSeconds <= 0 || r2.err.RetryAfterSeconds > 15 {
		t.Fatalf("short cooldown follow-up = %+v, want 429 with Retry-After", r2.err)
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
	h.p.Account("acct0").RecordModelCall(session, agytest.UpstreamModel, &pool.ModelFailure{Code: http.StatusInternalServerError, Message: "stale"})
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
	account.RecordModelCall(session, agytest.UpstreamModel, &pool.ModelFailure{Code: http.StatusBadGateway, Message: "Cloud Code request failed: context canceled"})

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
	if rec := h.records.all(t, 1)[0]; rec.Err == nil || rec.Err.Kind != conv.ErrCancelled || rec.Err.Message != "client disconnected" {
		t.Fatalf("disconnect record = %+v, want ErrCancelled client disconnected", rec)
	}
	for len(h.requests("session/cancel")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session/cancel never sent")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAdminCancelTurnReportsCancelled(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	done := make(chan result, 1)
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
	sessionID := h.requests("session/prompt")[0].Params.SessionID
	if err := h.gw.CancelTurn("acct0", sessionID); err != nil {
		t.Fatalf("CancelTurn = %v", err)
	}
	r := <-done
	if r.err == nil || r.err.Kind != conv.ErrCancelled || r.err.Message != "turn cancelled by the administrator" {
		t.Fatalf("admin cancel = %+v, want ErrCancelled by administrator", r)
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
	if recs := h.records.all(t, 3); recs[0].Anchor != AnchorNone || recs[1].Anchor != AnchorInvalid || recs[2].Anchor != AnchorOK {
		t.Fatalf("anchor statuses = [%q, %q, %q], want [\"\", %q, %q]", recs[0].Anchor, recs[1].Anchor, recs[2].Anchor, AnchorInvalid, AnchorOK)
	}
}

func TestInspectAnchorStatuses(t *testing.T) {
	t1 := newTranscript([]conv.Message{user("hello")})
	if got := inspectAnchor(t1); got != AnchorNone {
		t.Fatalf("first turn = %q, want %q", got, AnchorNone)
	}

	reply1 := conv.Message{Role: conv.RoleAssistant, Text: "hi"}
	t2Base := newTranscript([]conv.Message{user("hello"), reply1, user("next")})
	if got := inspectAnchor(t2Base); got != AnchorMissing {
		t.Fatalf("missing anchor = %q, want %q", got, AnchorMissing)
	}

	a1 := anchor{Account: "acct0", SessionID: "s1", Count: 2, Hash: t2Base.hashes[2]}.encode()
	t2OK := newTranscript([]conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: "hi", Anchors: []string{a1}}, user("next")})
	if got := inspectAnchor(t2OK); got != AnchorOK {
		t.Fatalf("valid anchor = %q, want %q", got, AnchorOK)
	}

	// Edited history invalidates the anchor's prefix hash.
	t2Edited := newTranscript([]conv.Message{user("edited"), {Role: conv.RoleAssistant, Text: "hi", Anchors: []string{a1}}, user("next")})
	if got := inspectAnchor(t2Edited); got != AnchorInvalid {
		t.Fatalf("edited history anchor = %q, want %q", got, AnchorInvalid)
	}

	// Foreign or malformed anchors without any matching agr1. anchor report invalid.
	t2Foreign := newTranscript([]conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: "hi", Anchors: []string{"gAAAA-other"}}, user("next")})
	if got := inspectAnchor(t2Foreign); got != AnchorInvalid {
		t.Fatalf("foreign anchor = %q, want %q", got, AnchorInvalid)
	}

	// Multi-turn: keeping turn 1's anchor while dropping turn 2's reports stale.
	t3Stale := newTranscript([]conv.Message{
		user("hello"),
		{Role: conv.RoleAssistant, Text: "hi", Anchors: []string{a1}},
		user("next"),
		{Role: conv.RoleAssistant, Text: "sure"},
		user("last"),
	})
	if got := inspectAnchor(t3Stale); got != AnchorStale {
		t.Fatalf("stale anchor = %q, want %q", got, AnchorStale)
	}

	a2 := anchor{Account: "acct0", SessionID: "s1", Count: 4, Hash: t3Stale.hashes[4]}.encode()
	t3OK := newTranscript([]conv.Message{
		user("hello"),
		{Role: conv.RoleAssistant, Text: "hi", Anchors: []string{a1}},
		user("next"),
		{Role: conv.RoleAssistant, Text: "sure", Anchors: []string{a2}},
		user("last"),
	})
	if got := inspectAnchor(t3OK); got != AnchorOK {
		t.Fatalf("multi-turn valid anchor = %q, want %q", got, AnchorOK)
	}
}

// A key that continues another key's session through its anchor owns the
// worker from then on: the busy slot it takes is the one the turn's end
// frees, and the worker counts toward its cap.
func TestAnchoredWarmChargesTheCallingKey(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hello")}})
	if first.err != nil || first.anchor == "" {
		t.Fatalf("first = %+v", first)
	}

	one := 1
	bob := testCaller("bob")
	bob.MaxWorkers = &one
	anchored := []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")}
	anchored[0].Anchors = []string{first.anchor}
	warm := h.completeAs(ctx, bob, conv.Request{Model: "gemini-3.8-flash", Messages: anchored})
	if warm.err != nil {
		t.Fatalf("anchored warm = %+v", warm.err)
	}
	if recs := h.records.all(t, 2); recs[1].Mode != ModeWarm {
		t.Fatalf("anchored mode = %s, want warm", recs[1].Mode)
	}

	if got := h.gw.Sessions(); len(got) != 1 || got[0].KeyHash != bob.KeyHash {
		t.Fatalf("sessions after the warm turn = %+v, want the one worker owned by bob", got)
	}

	// Nothing runs for bob now, so his one-worker cap has room: a new
	// conversation evicts the idle worker instead of failing.
	for i := 0; i < 3; i++ {
		r := h.completeAs(ctx, bob, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(fmt.Sprintf("later %d", i))}})
		if r.err != nil {
			t.Fatalf("request %d after the warm turn = %+v, want no limit error", i, r.err)
		}
	}
	if got := h.gw.Sessions(); len(got) != 1 {
		t.Fatalf("sessions = %+v, want 1 under bob's cap", got)
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
// not send them again on top: it rewinds to the user input before the
// tool-call model response and sends the assistant call + result as delta.
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
	if r := <-cancelled; r.err == nil || r.err.Kind != conv.ErrCancelled {
		t.Fatalf("first attempt = %+v, want ErrCancelled", r)
	}
	if retry.err != nil {
		t.Fatalf("retry = %+v", retry)
	}
	// Turn 1 left 2 steps (USER_INPUT + PLANNER_RESPONSE); turn 2's settled
	// user input added step 3 (USER_INPUT).
	h.assertRewound(h.requests("session/prompt")[0].Params.SessionID, 3)
	records := h.records.all(t, 4)
	if last := records[len(records)-1]; last.Mode != ModeRewind {
		t.Fatalf("retry mode = %q, want %q", last.Mode, ModeRewind)
	}
}

// When the relay already answered a tool result with the next tool_calls
// (parking the worker on step N+1) and the client never received that
// response, resending the same tool result must cancel the parked turn and
// rewind one step within the same session — never open a second worker to
// replay the conversation from scratch.
func TestDroppedToolCallsResponseRewindsSameSessionInsteadOfReplaying(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	req := func(messages []conv.Message) conv.Request {
		return conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools}
	}

	// Single long tool-loop turn with no prior finished turn:
	// U1 -> A1(tool_calls) -> T1 -> A2(tool_calls, lost by client).
	messages := []conv.Message{user("@toolchain read read read")}
	first := h.complete(ctx, "alice", req(messages))
	if first.err != nil || first.finish != conv.FinishToolCalls || first.anchor == "" {
		t.Fatalf("first = %+v", first)
	}
	messages = append(messages,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: first.calls, Anchors: []string{first.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Text: "res1"},
	)

	// Worker receives T1, emits A2(tool_calls), and parks waiting for T2.
	lostA2 := h.complete(ctx, "alice", req(messages))
	if lostA2.err != nil || lostA2.finish != conv.FinishToolCalls || lostA2.anchor == "" {
		t.Fatalf("lostA2 = %+v", lostA2)
	}
	if !h.turnIs(TurnParked) {
		t.Fatalf("sessions = %+v, want 1 parked worker", h.gw.Sessions())
	}

	// Client never got lostA2 and resends the exact same [U1, A1, T1]
	// request (even under another API key via A1's anchor).
	retried := h.complete(ctx, "bob", req(messages))
	if retried.err != nil {
		t.Fatalf("retried = %+v", retried)
	}
	if n := len(h.requests("session/new")); n != 1 {
		t.Fatalf("session/new count = %d, want 1 (must reuse session, not replay)", n)
	}
	sessionID := h.requests("session/prompt")[0].Params.SessionID
	// Rewound to U1 (1 step) because A2's turn never had an earlier settled T;
	// now run a deeper step (T2 lost after A3 parked) to verify rewinding to T1 (3 steps).
	h.assertRewound(sessionID, 1)
	if sessions := h.gw.Sessions(); len(sessions) != 1 {
		t.Fatalf("live sessions = %+v, want 1", sessions)
	}
	recs := h.records.all(t, 3)
	if recs[2].Mode != ModeRewind || recs[2].Anchor != AnchorOK || recs[2].SessionID != sessionID {
		t.Fatalf("retry record = %+v, want ModeRewind AnchorOK on %s", recs[2], sessionID)
	}

	// Now advance two tool steps in a fresh session and lose the third
	// tool_calls response: retry of T2 must rewind to T1 (3 steps: U1 + P1 + G1),
	// preserving the first tool call's native trajectory.
	m2 := []conv.Message{user("@toolchain read read read")}
	c1 := h.complete(ctx, "carol", req(m2))
	m2 = append(m2,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: c1.calls, Anchors: []string{c1.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: c1.calls[0].ID, ToolName: "read", Text: "r1"},
	)
	c2 := h.complete(ctx, "carol", req(m2))
	m2 = append(m2,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: c2.calls, Anchors: []string{c2.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: c2.calls[0].ID, ToolName: "read", Text: "r2"},
	)
	lostC3 := h.complete(ctx, "carol", req(m2))
	if c1.err != nil || c2.err != nil || lostC3.err != nil || lostC3.finish != conv.FinishToolCalls {
		t.Fatalf("carol setup: %+v %+v %+v", c1, c2, lostC3)
	}
	carolPromptsBefore := len(h.requests("session/prompt"))
	retryT2 := h.complete(ctx, "carol", req(m2))
	if retryT2.err != nil {
		t.Fatalf("retryT2 = %+v", retryT2)
	}
	if n := len(h.requests("session/new")); n != 2 {
		t.Fatalf("session/new count = %d, want 2 (no replay worker opened)", n)
	}
	carolPrompts := h.requests("session/prompt")
	carolSession := carolPrompts[carolPromptsBefore].Params.SessionID
	h.assertRewound(carolSession, 3)
	delta := carolPrompts[len(carolPrompts)-1].Params.Prompt[0]
	if delta.Resource == nil || !strings.Contains(delta.Resource.Text, "r2") || strings.Contains(delta.Resource.Text, "r1") {
		t.Fatalf("delta = %+v, want only [A2, T2(r2)] without resending r1", delta)
	}
}

// Serial tool calls from a single model call and client tool calls preceded
// by native agy tools must not record a bogus mid-step rewind position.
func TestSerialAndNativeToolStepsDoNotRecordInvalidInputSnapshot(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	req := func(messages []conv.Message) conv.Request {
		return conv.Request{Model: "gemini-3.8-flash", Messages: messages, Tools: tools}
	}

	// 1. Serial tool calls: 1 PLANNER_RESPONSE + 2 GENERIC steps.
	// Step 1 records U1 (steps=1). Step 2 has 0 new model calls -> inputSteps=0,
	// so retrying after step 2 rewinds cleanly to U1 (steps=1), never to
	// step 3 (middle of pending serial GENERIC steps).
	mSerial := []conv.Message{user("@serial read read")}
	s1 := h.complete(ctx, "alice", req(mSerial))
	mSerial = append(mSerial,
		conv.Message{Role: conv.RoleAssistant, ToolCalls: s1.calls, Anchors: []string{s1.anchor}},
		conv.Message{Role: conv.RoleTool, ToolCallID: s1.calls[0].ID, Text: "sr1"},
	)
	s2 := h.complete(ctx, "alice", req(mSerial))
	if s1.err != nil || s2.err != nil || s2.finish != conv.FinishToolCalls {
		t.Fatalf("serial: %+v %+v", s1, s2)
	}
	retrySerial := h.complete(ctx, "alice", req(mSerial))
	if retrySerial.err != nil {
		t.Fatalf("retrySerial = %+v", retrySerial)
	}
	h.assertRewound(h.requests("session/prompt")[0].Params.SessionID, 1)

	// 2. Native tool before client tool: 2 PLANNER_RESPONSE calls in one range
	// -> inputSteps=0, so U1 is not marked rewindable (native tool side effects
	// already ran on top of U1).
	mNative := []conv.Message{user("@nativetool read {}")}
	n1 := h.complete(ctx, "bob", req(mNative))
	if n1.err != nil || n1.finish != conv.FinishToolCalls {
		t.Fatalf("native: %+v", n1)
	}
	sessionID := h.gw.Sessions()[0].ID
	for _, s := range h.gw.Sessions() {
		if s.KeyHash == "hash-bob" {
			sessionID = s.ID
		}
	}
	if _, ok := h.gw.snaps.position("acct0", sessionID, 1, newTranscript(mNative).hashes[1]); ok {
		t.Fatal("input before native+client tool calls was marked rewindable")
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

func TestRecordsNameTheirSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	model := "gemini-3.8-flash"
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user("hello")}})
	h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")}})

	records := h.records.all(t, 2)
	if records[0].SessionID == "" || records[1].Mode != ModeWarm || records[1].SessionID != records[0].SessionID {
		t.Errorf("sessions = %q (%s), %q (%s), want one session for both", records[0].SessionID, records[0].Mode, records[1].SessionID, records[1].Mode)
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

	// acct1 now holds 1 idle worker (reaching TurnsPerAccount=1) while acct0
	// is empty, so the next conversation goes to acct0 instead of evicting
	// acct1's idle worker.
	second := h.complete(ctx, "dave", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("second")}})
	if second.err != nil || len(h.gw.Sessions()) != 2 {
		t.Fatalf("second = %+v, sessions = %+v; want 2 live sessions across both accounts", second.err, h.gw.Sessions())
	}
	if got := h.records.all(t, 2)[1]; got.Account != "acct0" {
		t.Fatalf("second account = %s, want acct0 (acct1 full with idle worker)", got.Account)
	}

	// Both accounts are now full (1 idle worker each). Park a tool call:
	// acct1 wins by priority (evicting its idle worker), and while acct1's
	// slot is busy the next conversation goes to acct0.
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
	recs := h.records.all(t, 5)
	if recs[2].Account != "acct1" || recs[3].Account != "acct0" || recs[4].Account != "acct1" {
		t.Fatalf("accounts = %s, %s, %s; want acct1, acct0, acct1", recs[2].Account, recs[3].Account, recs[4].Account)
	}

	// Limit acct1 to gemini-3.8-flash: claude skips acct1 and runs on acct0.
	acct1.SetRouting(5, []string{"gemini-3.8-flash"})
	if r := h.complete(ctx, alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("claude")}}); r.err != nil {
		t.Fatalf("claude = %+v", r.err)
	}
	if got := h.records.all(t, 6)[5]; got.Account != "acct0" {
		t.Fatalf("claude account = %s, want acct0", got.Account)
	}

	// Disallow gemini-3.8-flash on acct1: alice's warm session on acct1
	// cannot continue there. The client is told, and its retry replays on
	// acct0.
	acct1.SetRouting(5, []string{"claude-opus-5-5"})
	continued := conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("hello"), {Role: conv.RoleAssistant, Text: first.text}, user("again")},
	}
	if cont := h.complete(ctx, alice, continued); cont.err == nil {
		t.Fatal("a conversation whose account no longer offers the model continued anyway")
	}
	if cont := h.complete(ctx, alice, continued); cont.err != nil {
		t.Fatalf("continue = %+v", cont.err)
	}
	if got := h.records.all(t, 8)[7]; got.Account != "acct0" || got.Mode != ModeReplay {
		t.Fatalf("disallowed warm = %s %s, want acct0 replay", got.Account, got.Mode)
	}

	// A model disallowed on every account is not offered, even when an
	// out-of-rotation account has not reported its catalog yet.
	acct0.SetRouting(0, []string{"gemini-3.8-flash"})
	acct1.SetRouting(5, []string{"gemini-3.8-flash"})
	pausedAcct, err := h.p.Add(pool.AccountConfig{ID: "acct-paused", Name: "acct-paused", Home: filepath.Join(t.TempDir(), "paused")})
	if err != nil {
		t.Fatal(err)
	}
	pausedAcct.SetPaused(true)
	for _, m := range h.gw.Models() {
		if m.ID == "claude-opus-5-5" {
			t.Fatalf("disallowed model still in Models(): %+v", h.gw.Models())
		}
	}
	if r := h.complete(ctx, alice, conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("no")}}); r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("disallowed on all accounts = %+v, want ErrUnknownModel", r)
	}
	h.p.Remove("acct-paused")

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

func TestModelsExcludesOutOfRotationAccounts(t *testing.T) {
	modelIDs := func(gw *Gateway) []string {
		var out []string
		for _, m := range gw.Models() {
			out = append(out, m.ID+fmt.Sprint(m.Efforts))
		}
		return out
	}

	// Zero accounts: nothing in rotation, so Models() is empty.
	empty := newHarness(t, 0, nil)
	if got := empty.gw.Models(); len(got) != 0 {
		t.Fatalf("Models() with no accounts = %+v, want empty", got)
	}

	h := newHarness(t, 2, nil)
	acct0, acct1 := h.p.Account("acct0"), h.p.Account("acct1")

	// Unreported accounts that are all out of rotation must not expose fallback models.
	acct0.SetPaused(true)
	acct1.SetVerification(true, "https://accounts.google.com/v")
	if got := h.gw.Models(); len(got) != 0 {
		t.Fatalf("Models() with only unreported out-of-rotation accounts = %+v, want empty", got)
	}
	acct0.SetPaused(false)
	acct1.SetVerification(false, "")
	if got := h.gw.Models(); len(got) == 0 {
		t.Fatal("Models() with unreported in-rotation accounts = empty, want fallback catalog")
	}

	// Reported catalogs: acct1 uniquely offers claude-opus-5-5 and the medium tier of flash.
	acct0.SetModels([]agy.ModelInfo{
		{ModelID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
		{ModelID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
	})
	acct1.SetModels([]agy.ModelInfo{
		{ModelID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
		{ModelID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)"},
	})

	both := []string{"gemini-3.8-flash[low medium high]", "claude-opus-5-5[high]"}
	onlyAcct0 := []string{"gemini-3.8-flash[low high]"}
	if got := modelIDs(h.gw); !slices.Equal(got, both) {
		t.Fatalf("both in rotation = %v, want %v", got, both)
	}

	// Every out-of-rotation state excludes acct1's models and tiers.
	checks := []struct {
		name  string
		off   func()
		reset func()
	}{
		{"paused", func() { acct1.SetPaused(true) }, func() { acct1.SetPaused(false) }},
		{"disabled", func() { acct1.Disable(errors.New("sign in")) }, func() { acct1.Enable() }},
		{"blocked", func() { acct1.SetBlocked(errors.New("no proxy")) }, func() { acct1.SetBlocked(nil) }},
		{"verify", func() { acct1.SetVerification(true, "https://accounts.google.com/v") }, func() { acct1.SetVerification(false, "") }},
	}
	for _, tc := range checks {
		tc.off()
		if got := modelIDs(h.gw); !slices.Equal(got, onlyAcct0) {
			t.Fatalf("acct1 %s: Models() = %v, want %v", tc.name, got, onlyAcct0)
		}
		tc.reset()
		if got := modelIDs(h.gw); !slices.Equal(got, both) {
			t.Fatalf("acct1 restored from %s: Models() = %v, want %v", tc.name, got, both)
		}
	}

	// All reported accounts out of rotation: Models() is empty.
	acct0.SetPaused(true)
	acct1.SetPaused(true)
	if got := h.gw.Models(); len(got) != 0 {
		t.Fatalf("all paused: Models() = %+v, want empty", got)
	}
}

func TestSingleKeyDistributesNewSessionsAcrossAccounts(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.p.SetTurnsPerAccount(64)
	ctx := context.Background()

	seen := map[string]int{}
	for i := range 32 {
		prompt := fmt.Sprintf("session-%d", i)
		first := h.complete(ctx, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(prompt)}})
		if first.err != nil {
			t.Fatalf("session %d = %+v", i, first.err)
		}
		recs := h.records.all(t, 2*i+1)
		openedOn := recs[2*i].Account
		seen[openedOn]++

		warm := h.complete(ctx, "alice", conv.Request{
			Model:    "gemini-3.8-flash",
			Messages: []conv.Message{user(prompt), {Role: conv.RoleAssistant, Text: first.text}, user("follow-up")},
		})
		if warm.err != nil {
			t.Fatalf("session %d warm = %+v", i, warm.err)
		}
		warmRec := h.records.all(t, 2*i+2)[2*i+1]
		if warmRec.Account != openedOn || warmRec.Mode != ModeWarm {
			t.Fatalf("session %d warm = %s %s, want %s warm", i, warmRec.Account, warmRec.Mode, openedOn)
		}
		if len(seen) == 2 {
			return
		}
	}
	t.Fatalf("single key only routed new sessions to %v across 32 sessions; want both accounts used", seen)
}

func TestUnsupportedAccountSlotUntouched(t *testing.T) {
	h := newHarness(t, 2, map[int][]string{
		0: {"FAKE_AGY_MODELS=gemini-3.8-flash-low"},
		1: {"FAKE_AGY_MODELS=gemini-3.8-flash-low,claude-opus-5-5-high"},
	})
	h.p.SetTurnsPerAccount(1)
	ctx := context.Background()
	acct0, acct1 := h.p.Account("acct0"), h.p.Account("acct1")
	acct0.SetRouting(10, nil)
	acct1.SetRouting(0, nil)

	// Populate both accounts' catalogs.
	if r := h.complete(ctx, "seed0", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("s0")}}); r.err != nil {
		t.Fatalf("seed0 = %+v", r.err)
	}
	if r := h.complete(ctx, "seed1", conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("s1")}}); r.err != nil {
		t.Fatalf("seed1 = %+v", r.err)
	}

	// Hold acct1's single slot busy on a parked tool call; a concurrent
	// claude-opus-5-5 request must fail without ever claiming acct0's slot,
	// leaving acct0 free to serve gemini-3.8-flash throughout.
	tools := []conv.ToolSpec{readTool}
	parked := h.complete(ctx, "holder", conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user(`@tool read {}`)}, Tools: tools})
	if parked.err != nil || parked.finish != conv.FinishToolCalls {
		t.Fatalf("parked = %+v", parked)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			_ = h.complete(ctx, fmt.Sprintf("c-%d", i), conv.Request{Model: "claude-opus-5-5", Messages: []conv.Message{user("busy")}})
		})
	}
	if r := h.complete(ctx, "flash-user", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("ok")}}); r.err != nil {
		t.Fatalf("flash while claude busy = %+v", r.err)
	}
	wg.Wait()
	recs := h.records.all(t, 12)
	for _, rec := range recs {
		if rec.Model == "claude-opus-5-5" && rec.Account == "acct0" {
			t.Fatalf("unsupported model routed to acct0: %+v", rec)
		}
		if rec.KeyName == "flash-user" && rec.Account != "acct0" {
			t.Fatalf("flash-user routed to %s instead of high-priority acct0", rec.Account)
		}
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
	// conversation (fresh or warm) fails with ErrKeyBusy without dropping
	// the warm snapshot, while continuing the parked turn succeeds.
	tools := []conv.ToolSpec{readTool}
	parked := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{user(`@tool read {}`)}})
	if parked.err != nil || parked.finish != conv.FinishToolCalls {
		t.Fatalf("parked = %+v", parked)
	}
	if blocked := h.completeAs(ctx, alice, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("other")}}); blocked.err == nil || blocked.err.Kind != conv.ErrKeyBusy {
		t.Fatalf("key busy fresh = %+v, want ErrKeyBusy", blocked)
	}
	if blockedWarm := h.completeAs(ctx, alice, conv.Request{
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{user("c2"), {Role: conv.RoleAssistant, Text: c2.text}, user("c2-2"), {Role: conv.RoleAssistant, Text: c2Warm.text}, user("c2-3")},
	}); blockedWarm.err == nil || blockedWarm.err.Kind != conv.ErrKeyBusy {
		t.Fatalf("key busy warm = %+v, want ErrKeyBusy", blockedWarm)
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

	// Per-account worker limit (TurnsPerAccount=1 across 2 accounts): new
	// conversations from one user fill both accounts before evicting an idle
	// worker, and once all accounts are full they rotate by oldest idle.
	h.gw.SetMaxWorkers(0)
	h.p.SetTurnsPerAccount(1)
	s1 := h.complete(ctx, "dave", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("s1")}})
	s2 := h.complete(ctx, "dave", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("s2")}})
	if s1.err != nil || s2.err != nil || len(h.gw.Sessions()) != 2 {
		t.Fatalf("s1=%+v s2=%+v sessions=%+v; want 2 idle sessions across 2 accounts", s1.err, s2.err, h.gw.Sessions())
	}
	s3 := h.complete(ctx, "dave", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("s3")}})
	s4 := h.complete(ctx, "dave", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("s4")}})
	if s3.err != nil || s4.err != nil || len(h.gw.Sessions()) != 2 {
		t.Fatalf("s3=%+v s4=%+v sessions=%+v; want 2 sessions after evicting oldest on each account", s3.err, s4.err, h.gw.Sessions())
	}
	allRecs := h.records.all(t, 17)
	if allRecs[13].Account == allRecs[14].Account || allRecs[15].Account != allRecs[13].Account || allRecs[16].Account != allRecs[14].Account {
		t.Fatalf("dave accounts = %s, %s, %s, %s; want fill both then rotate", allRecs[13].Account, allRecs[14].Account, allRecs[15].Account, allRecs[16].Account)
	}
}

func TestEvictOldestWaitingWorkerWhenFull(t *testing.T) {
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	parkMsg := func(tag string) conv.Message { return user(`@tool read {"tag":"` + tag + `"}`) }

	t.Run("key_cap_oldest_first_and_running_protected", func(t *testing.T) {
		h := newHarness(t, 2, nil)
		two := 2
		bot := testCaller("bot")
		bot.MaxWorkers = &two
		bot.EvictWaiting = true

		// Park two conversations in order: p1 (oldest) then p2 (newer).
		p1Req := conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{parkMsg("p1")}}
		p2Req := conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{parkMsg("p2")}}
		p1 := h.completeAs(ctx, bot, p1Req)
		time.Sleep(2 * time.Millisecond)
		p2 := h.completeAs(ctx, bot, p2Req)
		if p1.err != nil || p1.finish != conv.FinishToolCalls || p2.err != nil || p2.finish != conv.FinishToolCalls {
			t.Fatalf("park p1=%+v p2=%+v", p1, p2)
		}
		recsBefore := h.records.all(t, 2)
		p1Sess, p2Sess := recsBefore[0].SessionID, recsBefore[1].SessionID

		// Opening p3 hits MaxWorkers=2 and must evict p1 (oldest parked),
		// leaving p2 parked and total sessions capped at 2.
		p3 := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("p3")}})
		if p3.err != nil || p3.finish != conv.FinishStop {
			t.Fatalf("p3 = %+v", p3)
		}
		sessions := h.gw.Sessions()
		if len(sessions) != 2 {
			t.Fatalf("sessions after p3 = %+v, want 2", sessions)
		}
		sawP1, sawP2Parked := false, false
		for _, s := range sessions {
			if s.ID == p1Sess {
				sawP1 = true
			}
			if s.ID == p2Sess && s.Turn != nil && s.Turn.State == TurnParked {
				sawP2Parked = true
			}
		}
		if sawP1 || !sawP2Parked {
			t.Fatalf("sessions = %+v; want p1 (%s) evicted and p2 (%s) still parked", sessions, p1Sess, p2Sess)
		}

		// p2 still continues in place (ModeContinue).
		p2Cont := h.completeAs(ctx, bot, conv.Request{
			Model: "gemini-3.8-flash", Tools: tools,
			Messages: []conv.Message{
				parkMsg("p2"),
				{Role: conv.RoleAssistant, ToolCalls: p2.calls, Anchors: []string{p2.anchor}},
				{Role: conv.RoleTool, ToolCallID: p2.calls[0].ID, ToolName: "read", Text: "res2"},
			},
		})
		if p2Cont.err != nil || p2Cont.finish != conv.FinishStop {
			t.Fatalf("p2Cont = %+v", p2Cont)
		}
		if rec := h.records.all(t, 4)[3]; rec.Mode != ModeContinue || rec.SessionID != p2Sess {
			t.Fatalf("p2Cont rec = %+v, want ModeContinue on %s", rec, p2Sess)
		}

		// Follow-up on the evicted p1 reopens it from disk and rewinds past the
		// cancelled dangling tool call (movedPast dropped the Steps=0 position).
		p1Follow := h.completeAs(ctx, bot, conv.Request{
			Model: "gemini-3.8-flash", Tools: tools,
			Messages: []conv.Message{
				parkMsg("p1"),
				{Role: conv.RoleAssistant, ToolCalls: p1.calls, Anchors: []string{p1.anchor}},
				{Role: conv.RoleTool, ToolCallID: p1.calls[0].ID, ToolName: "read", Text: "res1"},
			},
		})
		if p1Follow.err != nil || p1Follow.finish != conv.FinishStop {
			t.Fatalf("p1Follow = %+v", p1Follow)
		}
		if rec := h.records.all(t, 5)[4]; rec.Mode != ModeRewind || rec.SessionID != p1Sess {
			t.Fatalf("p1Follow rec = %+v, want ModeRewind on %s", rec, p1Sess)
		}

		// Running turns are never evicted even when EvictWaiting is enabled.
		one := 1
		bot.MaxWorkers = &one
		h.gw.TrimKeyIdle(bot.KeyHash, bot.MaxWorkers)
		hangCtx, cancelHang := context.WithCancel(ctx)
		hangDone := make(chan result, 1)
		go func() {
			hangDone <- h.completeAs(hangCtx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("@hang")}})
		}()
		for !h.turnIs(TurnRunning) {
			time.Sleep(5 * time.Millisecond)
		}
		blocked := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("blocked")}})
		if blocked.err == nil || blocked.err.Kind != conv.ErrKeyBusy {
			t.Fatalf("while running = %+v, want ErrKeyBusy", blocked)
		}
		cancelHang()
		<-hangDone
	})

	t.Run("warm_resume_and_toggle", func(t *testing.T) {
		h := newHarness(t, 1, nil)
		one := 1
		caller := testCaller("toggle")
		caller.MaxWorkers = &one

		// Seed an idle conversation w1, then park w2 (which evicts idle w1 to disk).
		w1 := h.completeAs(ctx, caller, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("w1")}})
		if w1.err != nil {
			t.Fatal(w1.err)
		}
		w2 := h.completeAs(ctx, caller, conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{parkMsg("w2")}})
		if w2.err != nil || w2.finish != conv.FinishToolCalls {
			t.Fatalf("w2 = %+v", w2)
		}

		// With EvictWaiting off, warm-resuming w1 fails with ErrKeyBusy.
		w1FollowReq := conv.Request{
			Model:    "gemini-3.8-flash",
			Messages: []conv.Message{user("w1"), {Role: conv.RoleAssistant, Text: w1.text}, user("w1-2")},
		}
		if blocked := h.completeAs(ctx, caller, w1FollowReq); blocked.err == nil || blocked.err.Kind != conv.ErrKeyBusy {
			t.Fatalf("before toggle = %+v, want ErrKeyBusy", blocked)
		}

		// Enabling EvictWaiting lets this key's next request evict its own
		// already-parked w2 so warm-resuming w1 (claimSession) succeeds.
		caller.EvictWaiting = true
		resumed := h.completeAs(ctx, caller, w1FollowReq)
		if resumed.err != nil || resumed.finish != conv.FinishStop {
			t.Fatalf("after toggle = %+v", resumed)
		}
		recs := h.records.all(t, 4)
		if recs[3].Mode != ModeWarm || recs[3].SessionID != recs[0].SessionID {
			t.Fatalf("resumed rec = %+v, want ModeWarm on %s", recs[3], recs[0].SessionID)
		}
		if got := h.gw.Sessions(); len(got) != 1 || got[0].ID != recs[0].SessionID {
			t.Fatalf("sessions after warm evict = %+v, want only %s", got, recs[0].SessionID)
		}
	})

	t.Run("account_switch_idle_first_and_strict_key_isolation", func(t *testing.T) {
		h := newHarness(t, 2, nil)
		h.p.SetTurnsPerAccount(1)

		bot := testCaller("bot")
		bot.EvictWaiting = true
		bot2 := testCaller("bot2")
		bot2.EvictWaiting = true
		normal := testCaller("normal")

		// Park a1 on one account; the other account still has a free slot.
		a1 := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{parkMsg("a1")}})
		if a1.err != nil || a1.finish != conv.FinishToolCalls {
			t.Fatalf("a1 = %+v", a1)
		}
		a1Rec := h.records.all(t, 1)[0]
		a1Acct, a1Sess := a1Rec.Account, a1Rec.SessionID

		// Next request finishes as idle on the OTHER account (free slot first),
		// leaving a1 parked untouched.
		idle1 := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("idle-1")}})
		if idle1.err != nil {
			t.Fatalf("idle1 = %+v", idle1.err)
		}
		otherAcct := h.records.all(t, 2)[1].Account
		if otherAcct == a1Acct {
			t.Fatalf("idle1 picked %s instead of free other account", otherAcct)
		}

		// Now otherAcct has an idle worker and a1Acct has a parked turn: next
		// request must evict otherAcct's idle worker before touching a1.
		a2 := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Tools: tools, Messages: []conv.Message{parkMsg("a2")}})
		if a2.err != nil || a2.finish != conv.FinishToolCalls {
			t.Fatalf("a2 = %+v", a2)
		}
		if rec := h.records.all(t, 3)[2]; rec.Account != otherAcct {
			t.Fatalf("a2 picked %s instead of evicting idle on %s", rec.Account, otherAcct)
		}
		a1StillParked := false
		for _, s := range h.gw.Sessions() {
			if s.ID == a1Sess && s.Turn != nil && s.Turn.State == TurnParked {
				a1StillParked = true
			}
		}
		if !a1StillParked {
			t.Fatalf("a1 (%s) was evicted before idle worker: %+v", a1Sess, h.gw.Sessions())
		}

		// Both accounts are now full with bot's parked turns (a1 oldest, a2 newer).
		// Neither a normal key nor another EvictWaiting key may evict bot's turns.
		for _, other := range []conv.Caller{normal, bot2} {
			blocked := h.completeAs(ctx, other, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("other-" + other.Name)}})
			if blocked.err == nil || blocked.err.Kind != conv.ErrUnavailable {
				t.Fatalf("%s against bot's parked turns = %+v, want ErrUnavailable", other.Name, blocked)
			}
		}

		// Pause a1Acct (holding bot's oldest parked turn): bot's own request
		// must skip the paused account and evict bot's a2 on otherAcct.
		h.p.Account(a1Acct).SetPaused(true)
		a3 := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("a3")}})
		if a3.err != nil || a3.finish != conv.FinishStop {
			t.Fatalf("a3 = %+v", a3)
		}
		if rec := h.records.all(t, 6)[5]; rec.Account != otherAcct {
			t.Fatalf("a3 account = %s, want %s (skipped paused %s)", rec.Account, otherAcct, a1Acct)
		}

		// Unpause a1Acct and cap global workers to 1 while bot holds the single
		// busy slot (a1 parked on a1Acct): other keys still get ErrUnavailable,
		// while bot itself evicts its own parked turn and succeeds.
		h.p.Account(a1Acct).SetPaused(false)
		h.gw.SetMaxWorkers(1)
		for _, other := range []conv.Caller{normal, bot2} {
			blocked := h.completeAs(ctx, other, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("global-" + other.Name)}})
			if blocked.err == nil || blocked.err.Kind != conv.ErrUnavailable {
				t.Fatalf("%s on global cap = %+v, want ErrUnavailable", other.Name, blocked)
			}
		}
		botGlobal := h.completeAs(ctx, bot, conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("global-bot")}})
		if botGlobal.err != nil || botGlobal.finish != conv.FinishStop {
			t.Fatalf("botGlobal = %+v", botGlobal)
		}
		if got := h.gw.Sessions(); len(got) != 1 {
			t.Fatalf("sessions after botGlobal = %+v, want 1", got)
		}
	})
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

	// Process killed mid-turn: outcome.Err does not leak process/stderr details.
	h.p.Account("acct0").SetPaused(false)
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

func TestUpstreamErrorsPassThrough(t *testing.T) {
	ctx := context.Background()
	req := conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}}

	singleCases := []struct {
		name       string
		code       string
		msg        string
		wantKind   conv.ErrorKind
		wantStatus int
		wantMsg    string
	}{
		{
			name:     "400 invalid argument",
			code:     "400",
			msg:      "INVALID_ARGUMENT (code 400): unsupported image",
			wantKind: conv.ErrInvalidRequest,
			wantMsg:  "INVALID_ARGUMENT (code 400): unsupported image",
		},
		{
			name:     "400 unsupported location passes message through when last account",
			code:     "400",
			msg:      "FAILED_PRECONDITION (code 400): User location is not supported for the API use.",
			wantKind: conv.ErrInvalidRequest,
			wantMsg:  "FAILED_PRECONDITION (code 400): User location is not supported for the API use.",
		},
		{
			name:       "413 payload too large",
			code:       "413",
			msg:        "HTTP 413 Request Entity Too Large",
			wantKind:   conv.ErrContextLength,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantMsg:    "HTTP 413 Request Entity Too Large",
		},
		{
			name:       "422 unprocessable entity",
			code:       "422",
			msg:        "HTTP 422: bad schema",
			wantKind:   conv.ErrInvalidRequest,
			wantStatus: http.StatusUnprocessableEntity,
			wantMsg:    "HTTP 422: bad schema",
		},
		{
			name:     "403 permission denied",
			code:     "403",
			msg:      "PERMISSION_DENIED (code 403): caller lacks entitlement",
			wantKind: conv.ErrForbidden,
			wantMsg:  "PERMISSION_DENIED (code 403): caller lacks entitlement",
		},
		{
			name:     "403 account banned passes message through when last account",
			code:     "403",
			msg:      "PERMISSION_DENIED (code 403): This service has been disabled in this account for violation of Terms of Service.",
			wantKind: conv.ErrForbidden,
			wantMsg:  "PERMISSION_DENIED (code 403): This service has been disabled in this account for violation of Terms of Service.",
		},
		{
			name:     "403 validation required hides verify",
			code:     "403",
			msg:      "PERMISSION_DENIED (code 403): VALIDATION_REQUIRED verify account",
			wantKind: conv.ErrUnknownModel,
		},
		{
			name:     "404 mapped to non-retryable model_not_found",
			code:     "404",
			msg:      "NOT_FOUND (code 404): model missing",
			wantKind: conv.ErrUnknownModel,
			wantMsg:  "NOT_FOUND (code 404): model missing",
		},
		{
			name:     "401 upstream hidden behind model unavailable",
			code:     "401",
			msg:      "UNAUTHENTICATED (code 401): bad token",
			wantKind: conv.ErrUnknownModel,
		},
		{
			name:     "503 service unavailable maps to overloaded",
			code:     "503",
			msg:      "UNAVAILABLE (code 503): backend down",
			wantKind: conv.ErrOverloaded,
			wantMsg:  "UNAVAILABLE (code 503): backend down",
		},
		{
			name:       "500 internal server error passes status",
			code:       "500",
			msg:        "INTERNAL (code 500): server boom",
			wantKind:   conv.ErrUpstream,
			wantStatus: http.StatusInternalServerError,
			wantMsg:    "INTERNAL (code 500): server boom",
		},
		{
			name:     "429 capacity without reset is overloaded",
			code:     "429",
			msg:      agytest.CapacityError,
			wantKind: conv.ErrOverloaded,
			wantMsg:  agytest.CapacityError,
		},
	}
	for _, tc := range singleCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_FAIL_CODE=" + tc.code, "FAKE_AGY_FAIL_MSG=" + tc.msg}})
			r := h.complete(ctx, "alice", req)
			if r.err == nil || r.err.Kind != tc.wantKind || r.err.HTTPStatus != tc.wantStatus {
				t.Fatalf("got err = %+v, want kind=%v status=%d", r.err, tc.wantKind, tc.wantStatus)
			}
			if tc.wantMsg != "" && r.err.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", r.err.Message, tc.wantMsg)
			}
			if strings.Contains(strings.ToLower(r.err.Message), "verify") {
				t.Fatalf("message leaked verify: %q", r.err.Message)
			}
		})
	}

	// Failures of the account itself that no retry fixes: the client gets
	// a retryable error with a short Retry-After, and its retry is served
	// by the second account.
	for _, tc := range []struct {
		name      string
		code      string
		msg       string
		wantInMsg string
	}{
		{"400-location", "400", "FAILED_PRECONDITION (code 400): User location is not supported for the API use.", "FAILED_PRECONDITION (code 400): User location is not supported for the API use."},
		{"401", "401", "UNAUTHENTICATED (code 401): bad token", ""},
		{"403-permission", "403", "PERMISSION_DENIED (code 403): caller lacks entitlement", ""},
		{"403-validation", "403", "PERMISSION_DENIED (code 403): VALIDATION_REQUIRED", ""},
		{"403-banned", "403", "PERMISSION_DENIED (code 403): This service has been disabled in this account for violation of Terms of Service.", "violation of Terms of Service"},
		{"404", "404", "NOT_FOUND (code 404): model missing", ""},
	} {
		t.Run("switch/"+tc.name, func(t *testing.T) {
			h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_FAIL_CODE=" + tc.code, "FAKE_AGY_FAIL_MSG=" + tc.msg}})
			user := userPreferring(t, h.p, "acct0")
			first := h.complete(ctx, user, req)
			if first.err == nil || first.err.Kind != conv.ErrOverloaded || first.err.HTTPStatus != 0 || first.err.RetryAfterSeconds != switchRetryAfterSeconds ||
				!strings.Contains(first.err.Message, "next request switches to another account") {
				t.Fatalf("first on %s = %+v, want a retryable 503 with the switch notice", tc.name, first.err)
			}
			if tc.wantInMsg != "" && !strings.Contains(first.err.Message, tc.wantInMsg) {
				t.Fatalf("first on %s message = %q, want upstream text %q", tc.name, first.err.Message, tc.wantInMsg)
			}
			if strings.Contains(strings.ToLower(first.err.Message), "verify") {
				t.Fatalf("message leaked verify: %q", first.err.Message)
			}
			if r := h.complete(ctx, user, req); r.err != nil || r.text != "reply to: hi" {
				t.Fatalf("retry on %s = %+v, want reply", tc.name, r)
			}
		})
	}

	// Failures that may pass count down the retries the account has left.
	for _, tc := range []struct {
		name string
		code string
		msg  string
	}{
		{"429-capacity", "429", agytest.CapacityError},
		{"503", "503", "UNAVAILABLE (code 503): backend down"},
	} {
		t.Run("count/"+tc.name, func(t *testing.T) {
			h := newHarness(t, 2, map[int][]string{0: {"FAKE_AGY_FAIL_CODE=" + tc.code, "FAKE_AGY_FAIL_MSG=" + tc.msg}})
			user := userPreferring(t, h.p, "acct0")
			for failures := 1; failures <= failuresBeforeSwitch; failures++ {
				r := h.complete(ctx, user, req)
				want := fmt.Sprintf("Retries left before switching accounts: %d", failuresBeforeSwitch-failures)
				if failures == failuresBeforeSwitch {
					want = fmt.Sprintf("(%d of %d). %s", failures, failuresBeforeSwitch, switchWarning)
				}
				if r.err == nil || !strings.Contains(r.err.Message, tc.msg) || !strings.Contains(r.err.Message, want) {
					t.Fatalf("failure %d on %s = %+v, want %q", failures, tc.name, r.err, want)
				}
			}
			if r := h.complete(ctx, user, req); r.err != nil || r.text != "reply to: hi" {
				t.Fatalf("request after the retries on %s = %+v, want reply", tc.name, r)
			}
		})
	}
}

func TestAuthErrorDisablingLastAccountIsNonRetryable(t *testing.T) {
	h := newHarness(t, 1, nil)
	tokenDir := filepath.Join(h.p.Account("acct0").GeminiDir(), "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "antigravity-oauth-token"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if r.err == nil || r.err.Kind != conv.ErrUnknownModel {
		t.Fatalf("err = %+v, want ErrUnknownModel (400 non-retryable)", r.err)
	}
}

func TestParkTTLExpiresAndReschedulesParkedTurns(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	// Default ParkTTL is 5m; lowering it reschedules an already-parked turn
	// and updates its reported ParkTimeout.
	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user(`@tool read {}`)}, Tools: tools})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}
	sessions := h.gw.Sessions()
	if len(sessions) != 1 || sessions[0].Turn == nil || sessions[0].Turn.State != TurnParked {
		t.Fatalf("sessions = %+v, want one parked turn", sessions)
	}
	if got := sessions[0].Turn.ParkTimeout.Sub(sessions[0].Turn.ParkedAt); got != defaultParkTTL {
		t.Fatalf("initial park timeout span = %v, want %v", got, defaultParkTTL)
	}

	h.gw.SetParkTTL(20 * time.Millisecond)
	sessions = h.gw.Sessions()
	if len(sessions) == 1 && sessions[0].Turn != nil {
		if got := sessions[0].Turn.ParkTimeout.Sub(sessions[0].Turn.ParkedAt); got != 20*time.Millisecond {
			t.Fatalf("updated park timeout span = %v, want 20ms", got)
		}
	}
	h.waitRun(0, false)

	// The session stays reusable via warm delta if the client returns later.
	resumed := h.complete(ctx, "alice", conv.Request{
		Model: model,
		Tools: tools,
		Messages: []conv.Message{
			user(`@tool read {}`),
			{Role: conv.RoleAssistant, ToolCalls: first.calls},
			{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, Text: "late result"},
		},
	})
	if resumed.err != nil || resumed.finish != conv.FinishStop {
		t.Fatalf("resumed after park timeout = %+v", resumed)
	}
	prompts := h.requests("session/prompt")
	if len(prompts) != 2 || prompts[0].Params.SessionID != prompts[1].Params.SessionID || prompts[1].Params.Prompt[0].Resource == nil || !strings.Contains(prompts[1].Params.Prompt[0].Resource.Text, "late result") {
		t.Fatalf("delta prompts = %+v", prompts)
	}
	if got := h.records.all(t, 2)[1]; got.Mode != ModeWarm {
		t.Fatalf("mode after park timeout = %q, want %q", got.Mode, ModeWarm)
	}
}

func TestCancelDuringProcStartAndConfigSetup(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{
		0: {"FAKE_AGY_INIT_DELAY=180ms", "FAKE_AGY_CONFIG_DELAY=150ms"},
	})

	// 1. Cancel while Account.Proc is still initializing: returns promptly
	// without waiting for init or opening a session.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel1()
	start := time.Now()
	h.complete(ctx1, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("hi")}})
	if elapsed := time.Since(start); elapsed > 140*time.Millisecond {
		t.Fatalf("cancel during init blocked %v", elapsed)
	}
	if rec := h.records.all(t, 1)[0]; rec.Err == nil || rec.Err.Kind != conv.ErrCancelled {
		t.Fatalf("cancel during init record = %+v, want ErrCancelled", rec)
	}

	// 2. Cancel during SetConfigOption: returns promptly and never sends session/prompt.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 260*time.Millisecond)
	defer cancel2()
	h.complete(ctx2, "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("second")}})
	if rec := h.records.all(t, 2)[1]; rec.Err == nil || rec.Err.Kind != conv.ErrCancelled {
		t.Fatalf("cancel during config setup record = %+v, want ErrCancelled", rec)
	}
	time.Sleep(200 * time.Millisecond)
	if prompts := h.requests("session/prompt"); len(prompts) != 0 {
		t.Fatalf("session/prompt sent despite early cancel: %+v", prompts)
	}

	// Subsequent normal request succeeds on the healthy account.
	ok := h.complete(context.Background(), "alice", conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user("ok")}})
	if ok.err != nil || ok.text != "reply to: ok" {
		t.Fatalf("subsequent request = %+v, want reply to: ok", ok)
	}
}

func TestCancelledContinueLeavesParkedTurnIntact(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user(`@tool read {}`)}, Tools: tools})
	if first.err != nil || len(first.calls) != 1 {
		t.Fatalf("first = %+v", first)
	}
	contMessages := []conv.Message{
		user(`@tool read {}`),
		{Role: conv.RoleAssistant, ToolCalls: first.calls},
		{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Text: "file body"},
	}

	// An already-cancelled continuation must not unpark or cancel the parked turn.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	h.complete(cancelledCtx, "alice", conv.Request{Model: model, Messages: contMessages, Tools: tools})
	if rec := h.records.all(t, 2)[1]; rec.Err == nil || rec.Err.Kind != conv.ErrCancelled {
		t.Fatalf("cancelled continue record = %+v, want ErrCancelled", rec)
	}
	sessions := h.gw.Sessions()
	if len(sessions) != 1 || sessions[0].Turn == nil || sessions[0].Turn.State != TurnParked {
		t.Fatalf("sessions after cancelled continue = %+v, want parked turn intact", sessions)
	}

	// A real continuation afterwards still resumes the parked turn in place.
	resumed := h.complete(ctx, "alice", conv.Request{Model: model, Messages: contMessages, Tools: tools})
	if resumed.err != nil || resumed.text != "tool said: file body" {
		t.Fatalf("resumed = %+v, want tool said: file body", resumed)
	}
	if recs := h.records.all(t, 3); recs[2].Mode != ModeContinue {
		t.Fatalf("resumed mode = %q, want %q", recs[2].Mode, ModeContinue)
	}
}

func TestCancelDuringSteerSettlesCleanly(t *testing.T) {
	ctx := context.Background()
	tools := []conv.ToolSpec{readTool}
	model := "gemini-3.8-flash"

	for i := range 6 {
		t.Run(fmt.Sprintf("iter-%d", i), func(t *testing.T) {
			h := newHarness(t, 1, nil)
			t.Cleanup(func() {
				procs := h.p.Account("acct0").LiveProcs()
				h.p.Close()
				for _, proc := range procs {
					<-proc.Done()
				}
			})
			prompt := fmt.Sprintf("@tool read {\"n\":%d}", i)
			first := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user(prompt)}, Tools: tools})
			if first.err != nil || len(first.calls) != 1 {
				t.Fatalf("first = %+v", first)
			}
			// Tool result "@hang" keeps the fake turn parked on a hang if steer
			// settles the tool call before cancel reaches agy, exposing any leaked
			// cancel immediately instead of letting the turn finish on its own.
			steerMessages := []conv.Message{
				user(prompt),
				{Role: conv.RoleAssistant, ToolCalls: first.calls},
				{Role: conv.RoleTool, ToolCallID: first.calls[0].ID, ToolName: "read", Text: "@hang"},
				user(fmt.Sprintf("steer %d", i)),
			}

			steerCtx, cancel := context.WithCancel(ctx)
			done := make(chan result, 1)
			go func() {
				done <- h.complete(steerCtx, "alice", conv.Request{Model: model, Messages: steerMessages, Tools: tools})
			}()
			if i == 0 {
				// Wait until the steer session/prompt is actually written to agy.
				deadline := time.Now().Add(2 * time.Second)
				for len(h.requests("session/prompt")) < 2 {
					if time.Now().After(deadline) {
						t.Fatal("steer session/prompt never sent")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				time.Sleep(time.Duration(i-1) * time.Millisecond)
			}
			cancel()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled steer did not return")
			}
			if rec := h.records.all(t, 2)[1]; rec.Err == nil || rec.Err.Kind != conv.ErrCancelled {
				t.Fatalf("steer cancel record = %+v, want ErrCancelled", rec)
			}
			sessions := h.gw.Sessions()
			if len(sessions) == 1 && sessions[0].Turn != nil && sessions[0].Turn.State == TurnParked {
				if i == 0 {
					t.Fatal("iter 0 cancelled after steer prompt was sent; turn must not stay parked")
				}
				// Cancel arrived before tryContinue unparked the turn: no steer prompt was sent.
				if got := len(h.requests("session/prompt")); got != 1 {
					t.Fatalf("parked turn left with %d prompts, want 1", got)
				}
			} else {
				h.waitRun(0, false)
			}

			// Account process must still be healthy and usable.
			ok := h.complete(ctx, "alice", conv.Request{Model: model, Messages: []conv.Message{user("still alive")}, Tools: tools})
			if ok.err != nil || ok.text != "reply to: still alive" {
				t.Fatalf("after steer cancel = %+v, want reply to: still alive", ok)
			}
		})
	}
}
