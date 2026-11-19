package gateway

import (
	"context"
	"testing"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/conv"
)

// stuckAfterCalls is pool's stuckCallStreak: the fake's "@stuck" fails
// more calls than that.
const stuckAfterCalls = 10

func directive(input string) conv.Request {
	return conv.Request{Model: "gemini-3.8-flash", Messages: []conv.Message{user(input)}}
}

// A session whose generation calls keep failing is stopped before agy
// retries without end; the request fails as its last call did, and the
// next request replays on a session of its own.
func TestStuckSessionIsStoppedAndFailsAsItsLastCall(t *testing.T) {
	h := newHarness(t, 1, nil)
	ctx := context.Background()

	r := h.complete(ctx, "alice", directive("@stuck"))
	if r.err == nil || r.err.Kind != conv.ErrUpstream || r.err.Message != agytest.StuckError {
		t.Fatalf("stuck request = %+v", r)
	}
	h.tokens.mu.Lock()
	served := len(h.tokens.list)
	h.tokens.mu.Unlock()
	if served != stuckAfterCalls {
		t.Fatalf("%d calls were served before the session was stopped, want %d", served, stuckAfterCalls)
	}

	next := h.complete(ctx, "alice", directive("hello"))
	if next.err != nil || next.text != "reply to: hello" {
		t.Fatalf("request after the stuck one = %+v", next)
	}
}

// A response Google's content filter stopped is refused for good: no
// other account is tried, and the error is no retryable upstream one.
func TestContentSafetyStopIsNotRetried(t *testing.T) {
	h := newHarness(t, 2, nil)

	r := h.complete(context.Background(), "alice", directive("@blocked"))
	if r.err == nil || r.err.Kind != conv.ErrInvalidRequest || r.err.Message != agytest.BlockedError {
		t.Fatalf("blocked request = %+v", r)
	}
	if n := len(h.requests("session/prompt")); n != 1 {
		t.Fatalf("%d prompts reached agy; a blocked response must not try another account", n)
	}
}

// A reply cut off at the output token limit is no failure: the client
// gets it with finish reason length, and compacts its conversation.
func TestOutputTokenLimitFinishesWithLength(t *testing.T) {
	h := newHarness(t, 1, nil)

	r := h.complete(context.Background(), "alice", directive("@maxtokens"))
	if r.err != nil || r.text != "cut" || r.finish != conv.FinishLength {
		t.Fatalf("result = %+v", r)
	}
}
