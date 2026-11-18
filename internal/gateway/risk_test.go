package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/sysprompt"
)

const riskSystem = "You are Opencode, an interactive CLI tool.\n\nHelp the user with software engineering tasks."

func riskRequest(system, input string) conv.Request {
	messages := []conv.Message{user(input)}
	if system != "" {
		messages = append([]conv.Message{{Role: conv.RoleSystem, Text: system}}, messages...)
	}
	return conv.Request{Model: "gemini-3.8-flash", Messages: messages}
}

// A request that keeps drawing 429s without a wait fails at once with a
// 403-class error, is not tried on the other account, and flags its system
// prompt; the flag then refuses that prompt, however it is formatted,
// until it is cancelled.
func TestRiskControlFlagsSystemPrompt(t *testing.T) {
	h := newHarness(t, 2, nil)
	ctx := context.Background()

	first := h.complete(ctx, "alice", riskRequest(riskSystem, "@risk429"))
	if first.err == nil || first.err.Kind != conv.ErrRiskControl || !strings.Contains(first.err.Message, "risk control") {
		t.Fatalf("risky request = %+v", first)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("%d prompts reached agy; a refused request must not try another account", n)
	}
	for _, account := range h.p.Accounts() {
		if cooling := account.Status().Cooldowns; len(cooling) != 0 {
			t.Fatalf("account %s cools down for risk control: %v", account.Name(), cooling)
		}
	}

	fingerprint := sysprompt.Fingerprint(riskSystem)
	h.flags.mu.Lock()
	flagged := append([]FlaggedPrompt(nil), h.flags.flagged...)
	h.flags.mu.Unlock()
	if len(flagged) != 1 {
		t.Fatalf("flagged = %+v", flagged)
	}
	got := flagged[0]
	if got.Fingerprint != fingerprint || got.Text != riskSystem || got.KeyHash != "hash-alice" || got.KeyName != "alice" ||
		got.Account == "" || got.Session == "" || got.Model != "gemini-3.8-flash" || got.Until.Sub(got.At) != flagDuration ||
		len(got.Request) == 0 || len(got.Response) == 0 {
		t.Fatalf("flagged prompt = %+v", got)
	}

	// The same prompt, reformatted, is refused before agy is asked, for
	// any key and with any input.
	reformatted := strings.ToUpper(strings.ReplaceAll(riskSystem, "\n\n", "\n"))
	for _, caller := range []string{"alice", "bob"} {
		r := h.complete(ctx, caller, riskRequest(reformatted, "hello"))
		if r.err == nil || r.err.Kind != conv.ErrRiskControl || !strings.Contains(r.err.Message, got.Until.UTC().Format(time.RFC3339)) {
			t.Fatalf("%s with the flagged prompt = %+v", caller, r)
		}
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("%d prompts reached agy after the refusals", n)
	}

	other := h.complete(ctx, "alice", riskRequest("Answer briefly.", "hello"))
	if other.err != nil || other.text != "reply to: hello" {
		t.Fatalf("another system prompt = %+v", other)
	}

	h.flags.cancel(fingerprint)
	again := h.complete(ctx, "alice", riskRequest(riskSystem, "hello"))
	if again.err != nil || again.text != "reply to: hello" {
		t.Fatalf("after the flag was cancelled = %+v", again)
	}
}

// Peers that keep serving show the request is the problem, so the second
// 429 is enough.
func TestRiskControlAfterSecondNoWaitWhenPeersServe(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	account := h.p.Accounts()[0]

	done := make(chan result, 1)
	go func() { done <- h.complete(ctx, "alice", riskRequest(riskSystem, "@risk429")) }()

	// Peer calls keep arriving while the risky request retries.
	deadline := time.After(10 * time.Second)
	var risky result
loop:
	for i := 0; ; i++ {
		select {
		case risky = <-done:
			break loop
		case <-deadline:
			t.Fatal("risky request never ended")
		default:
		}
		if r := h.complete(ctx, "bob", riskRequest("peer", "@ok")); r.err != nil || r.text != "ok" {
			t.Fatalf("peer %d = %+v", i, r)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if risky.err == nil || risky.err.Kind != conv.ErrRiskControl {
		t.Fatalf("risky request = %+v", risky)
	}
	if len(account.Status().Cooldowns) != 0 {
		t.Fatalf("cooldowns = %v", account.Status().Cooldowns)
	}
}

// Without a system prompt there is nothing to flag, and nothing flagged
// may then refuse every request that has none.
func TestRiskControlWithoutSystemPromptFlagsNothing(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()

	r := h.complete(ctx, "alice", riskRequest("", "@risk429"))
	if r.err == nil || r.err.Kind != conv.ErrRiskControl {
		t.Fatalf("risky request = %+v", r)
	}
	h.flags.mu.Lock()
	flagged := len(h.flags.flagged)
	h.flags.mu.Unlock()
	if flagged != 0 {
		t.Fatalf("%d prompts flagged", flagged)
	}
	if ok := h.complete(ctx, "alice", riskRequest("", "hello")); ok.err != nil || ok.text != "reply to: hello" {
		t.Fatalf("next request without a system prompt = %+v", ok)
	}
}

// A flag that cannot be saved is logged; the request still fails as a
// risk-control refusal, and the next one is refused too.
func TestRiskControlSurvivesFlagFailure(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.flags.err = errors.New("disk full")
	r := h.complete(context.Background(), "alice", riskRequest(riskSystem, "@risk429"))
	if r.err == nil || r.err.Kind != conv.ErrRiskControl {
		t.Fatalf("risky request = %+v", r)
	}
	if until, _ := h.flags.Flagged(sysprompt.Fingerprint(riskSystem)); until.IsZero() {
		t.Fatal("prompt not flagged in memory")
	}
}

// A pinned prompt is refused after its flag ended, with a message that
// names no end.
func TestPinnedFlaggedPromptIsRefusedAfterItsFlagEnded(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()
	fingerprint := sysprompt.Fingerprint(riskSystem)
	h.flags.mu.Lock()
	h.flags.until[fingerprint] = time.Now().Add(-time.Hour)
	h.flags.pinned[fingerprint] = true
	h.flags.mu.Unlock()

	r := h.complete(ctx, "alice", riskRequest(riskSystem, "hello"))
	if r.err == nil || r.err.Kind != conv.ErrRiskControl || r.err.Message != pinnedMessage {
		t.Fatalf("pinned prompt = %+v", r)
	}
	if n := len(h.requests("session/prompt")); n != 0 {
		t.Fatalf("%d prompts reached agy", n)
	}

	h.flags.mu.Lock()
	delete(h.flags.pinned, fingerprint)
	h.flags.mu.Unlock()
	if ok := h.complete(ctx, "alice", riskRequest(riskSystem, "hello")); ok.err != nil {
		t.Fatalf("after unpinning = %+v", ok)
	}
}
