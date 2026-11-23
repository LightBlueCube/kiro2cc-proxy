package gateway

import (
	"context"
	"errors"
	"testing"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

func TestWarmupRecordsCallAndCleansUpSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	account := h.p.Account("acct0")

	err := h.gw.Warmup(context.Background(), account, SystemCall{
		KeyHash: "system:warmup",
		KeyName: "warmup",
		Model:   "gemini-3.8-flash",
		Slug:    "gemini-3.8-flash-low",
		Effort:  "low",
		Group:   pool.GroupGemini,
	})
	if err != nil {
		t.Fatalf("Warmup failed: %v", err)
	}

	recs := h.records.all(t, 1)
	rec := recs[0]
	if rec.KeyHash != "system:warmup" || rec.KeyName != "warmup" || rec.API != "" || rec.Mode != "" || rec.KeyGroup != "" {
		t.Errorf("record identity = %+v", rec)
	}
	prompts := h.requests("session/prompt")
	if len(prompts) != 1 || len(prompts[0].Params.Prompt) != 1 || prompts[0].Params.Prompt[0].Text != "who are you" {
		t.Fatalf("prompts = %+v; want single %q prompt", prompts, "who are you")
	}
	if rec.Model != "gemini-3.8-flash" || rec.Slug != "gemini-3.8-flash-low" || rec.Effort != "low" || rec.QuotaGroup != pool.GroupGemini {
		t.Errorf("record model = %+v", rec)
	}
	if rec.Account != "acct0" || rec.Finish != conv.FinishStop || rec.Err != nil {
		t.Errorf("record outcome = %+v", rec)
	}
	if rec.Usage == nil || rec.Usage.PromptTokens == 0 || rec.Usage.CompletionTokens == 0 {
		t.Errorf("record usage = %+v, want non-zero", rec.Usage)
	}
	if rec.SessionID == "" || rec.CallToken == "" {
		t.Errorf("record session/callToken missing: %+v", rec)
	}
	if len(h.gw.Sessions()) != 0 {
		t.Errorf("sessions left open after warmup: %+v", h.gw.Sessions())
	}
	if account.Turns() != 0 {
		t.Errorf("account turns = %d, want 0", account.Turns())
	}
	if convs, err := agy.Conversations(account.GeminiDir()); err != nil || len(convs) != 0 {
		t.Errorf("warmup conversation files left behind: %v, %v", convs, err)
	}
}

func TestProbeReturnsReplyAndRecordsUnderCallerKey(t *testing.T) {
	h := newHarness(t, 1, nil)
	account := h.p.Account("acct0")

	reply, err := h.gw.Probe(context.Background(), account, SystemCall{
		KeyHash: "system:test",
		KeyName: "test",
		Model:   "gemini-3.8-flash",
		Slug:    "gemini-3.8-flash-low",
		Effort:  "low",
		Group:   pool.GroupGemini,
	})
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if reply != "reply to: who are you" {
		t.Errorf("reply = %q, want %q", reply, "reply to: who are you")
	}

	rec := h.records.all(t, 1)[0]
	if rec.KeyHash != "system:test" || rec.KeyName != "test" || rec.Err != nil {
		t.Errorf("record = %+v, want a successful system:test call", rec)
	}
	if len(h.gw.Sessions()) != 0 {
		t.Errorf("sessions left open after probe: %+v", h.gw.Sessions())
	}
}

func TestWarmupQuotaFailureCoolsGroupAndLinksCallToken(t *testing.T) {
	h := newHarness(t, 1, map[int][]string{0: {"FAKE_AGY_QUOTA=1"}})
	account := h.p.Account("acct0")

	err := h.gw.Warmup(context.Background(), account, SystemCall{
		KeyHash: "system:warmup",
		KeyName: "warmup",
		Model:   "claude-opus-5-5",
		Slug:    "claude-opus-5-5-high",
		Effort:  "high",
		Group:   pool.GroupClaudeGPT,
	})
	if err == nil {
		t.Fatal("expected quota error from Warmup")
	}

	if rem := account.CooldownRemaining(pool.GroupClaudeGPT); rem <= 0 {
		t.Errorf("claude-gpt cooldown = %v, want > 0", rem)
	}
	recs := h.records.all(t, 1)
	if recs[0].Err == nil || recs[0].Err.Kind != conv.ErrQuota {
		t.Fatalf("records = %+v, want one ErrQuota record", recs)
	}
	h.tokens.mu.Lock()
	tokens := append([]string(nil), h.tokens.list...)
	h.tokens.mu.Unlock()
	if len(tokens) == 0 || tokens[0] != recs[0].CallToken {
		t.Errorf("captured call tokens = %v, want %q", tokens, recs[0].CallToken)
	}
	if len(h.gw.Sessions()) != 0 {
		t.Errorf("sessions left open after failed warmup: %+v", h.gw.Sessions())
	}
}

func TestWarmupDefersWhenSlotsOccupiedWithoutEvictingIdleUserSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.gw.SetMaxWorkers(1)

	// Open a normal user session that becomes idle and fills the single worker slot.
	res := h.complete(context.Background(), "user1", conv.Request{
		API:      "chat",
		Model:    "gemini-3.8-flash",
		Messages: []conv.Message{{Role: conv.RoleUser, Text: "hello"}},
	})
	if res.err != nil {
		t.Fatalf("user request failed: %v", res.err)
	}
	before := h.gw.Sessions()
	if len(before) != 1 {
		t.Fatalf("expected 1 idle session, got %d", len(before))
	}

	err := h.gw.Warmup(context.Background(), h.p.Account("acct0"), SystemCall{
		KeyHash: "system:warmup",
		KeyName: "warmup",
		Model:   "gemini-3.8-flash",
		Slug:    "gemini-3.8-flash-low",
		Effort:  "low",
		Group:   pool.GroupGemini,
	})
	if !errors.Is(err, ErrSlotsBusy) {
		t.Fatalf("Warmup err = %v, want ErrSlotsBusy", err)
	}
	after := h.gw.Sessions()
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("idle user session was evicted: before=%+v after=%+v", before, after)
	}
	if recs := h.records.all(t, 1); len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (deferred warmup must not record a failure)", len(recs))
	}
}
