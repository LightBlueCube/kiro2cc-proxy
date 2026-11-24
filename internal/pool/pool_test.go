package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/agy/agytest"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProxyEnvAndBlocking(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	extra := []string{"X=1", "HTTPS_PROXY=http://ignored:1"}
	a, err := p.Add(AccountConfig{Name: "a", Home: filepath.Join(t.TempDir(), "a"), Env: extra, Proxy: "http://a:token@127.0.0.1:7890"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(extra), "HTTPS_PROXY=http://a:token@127.0.0.1:7890", "HTTP_PROXY=http://a:token@127.0.0.1:7890")
	if got := a.Env(); !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}

	a.SetBlocked(errors.New("no route"))
	if _, err := p.Pick("u", GroupGemini, nil, nil); err == nil || a.Status().Blocked != "no route" {
		t.Fatalf("blocked account picked: %v", err)
	}
	a.SetBlocked(nil)
	got, err := p.Pick("u", GroupGemini, nil, nil)
	if err != nil || got != a {
		t.Fatalf("unblocked pick = %v, %v", got, err)
	}
	a.Release()

	a.SetVerification(true, "https://accounts.google.com/x")
	if _, err := p.Pick("u", GroupGemini, nil, nil); !errors.Is(err, ErrNoAccount) || !a.Status().Verify || a.Status().VerifyURL != "https://accounts.google.com/x" {
		t.Fatalf("account awaiting verification picked: %v, %+v", err, a.Status())
	}
	a.SetVerification(false, "")
	if got, err := p.Pick("u", GroupGemini, nil, nil); err != nil || got != a {
		t.Fatalf("verified pick = %v, %v", got, err)
	}
	a.Release()

	a.Disable(errors.New("sign-in required"))
	if _, err := p.Pick("u", GroupGemini, nil, nil); !errors.Is(err, ErrNoAccount) || a.Status().Disabled == "" {
		t.Fatalf("disabled account picked: %v, %+v", err, a.Status())
	}
	a.Enable()
	if got, err := p.Pick("u", GroupGemini, nil, nil); err != nil || got != a || a.Status().Disabled != "" {
		t.Fatalf("enabled pick = %v, %v, %+v", got, err, a.Status())
	}
	a.Release()

	const locErr = "FAILED_PRECONDITION (code 400): User location is not supported for the API use."
	if !a.PauseWithReason(locErr) {
		t.Fatal("first PauseWithReason should report change")
	}
	if a.PauseWithReason(locErr) {
		t.Fatal("duplicate PauseWithReason should report no change")
	}
	if _, err := p.Pick("u", GroupGemini, nil, nil); !errors.Is(err, ErrNoAccount) || !a.Status().Paused || a.Status().PauseReason != locErr {
		t.Fatalf("auto-paused account picked: %v, %+v", err, a.Status())
	}
	if err := a.Available(GroupGemini); err == nil {
		t.Fatal("auto-paused account should be unavailable")
	}
	a.SetPaused(false)
	if got, err := p.Pick("u", GroupGemini, nil, nil); err != nil || got != a || a.Status().Paused || a.Status().PauseReason != "" {
		t.Fatalf("resumed pick = %v, %v, %+v", got, err, a.Status())
	}
	a.Release()
}

func TestPriorityAndAllowedModels(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	low, err := p.Add(AccountConfig{Name: "low", Home: filepath.Join(dir, "low")})
	if err != nil {
		t.Fatal(err)
	}
	high, err := p.Add(AccountConfig{Name: "high", Home: filepath.Join(dir, "high")})
	if err != nil {
		t.Fatal(err)
	}
	high.SetRouting(10, []string{"gemini-3.8-flash"})
	low.SetRouting(-1, nil)

	if !high.AllowsModel("gemini-3.8-flash") || high.AllowsModel("claude-opus-5-5") {
		t.Fatalf("high allowed = %v", high.AllowedModels())
	}
	if !low.AllowsModel("claude-opus-5-5") {
		t.Fatalf("low allowed = %v", low.AllowedModels())
	}

	// Higher priority wins for every user while a turn slot is free.
	for _, user := range []string{"u1", "u2", "u3", "u4"} {
		got, err := p.Pick(user, GroupGemini, nil, nil)
		if err != nil || got != high {
			t.Fatalf("pick(%s) = %v, %v; want high", user, got, err)
		}
		got.Release()
	}

	// With high's slot full, Pick moves to low.
	first, err := p.Pick("u1", GroupGemini, nil, nil)
	if err != nil || first != high {
		t.Fatalf("first pick = %v, %v", first, err)
	}
	second, err := p.Pick("u1", GroupGemini, nil, nil)
	if err != nil || second != low {
		t.Fatalf("second pick with high busy = %v, %v; want low", second, err)
	}
	second.Release()
	first.Release()
}

func TestQuotaGroup(t *testing.T) {
	for model, want := range map[string]string{
		"gemini-4-argon":    GroupGemini,
		"gemini-4-carbon":   GroupGemini,
		"gemini-3.8-flash":  GroupGemini,
		"gemini-4-hydrogen": GroupGemini,
		"gemini-4-neon":     GroupGemini,
		"claude-sonnet-5-5": GroupClaudeGPT,
		"gpt-oss-120b":      GroupClaudeGPT,
	} {
		if got := QuotaGroup(model); got != want {
			t.Errorf("QuotaGroup(%s) = %s, want %s", model, got, want)
		}
	}
}

func TestCooldownSetAndSync(t *testing.T) {
	p, err := New(Config{TurnsPerAccount: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Add(AccountConfig{Name: "a", Home: filepath.Join(t.TempDir(), "a")})
	if err != nil {
		t.Fatal(err)
	}

	longUntil := time.Now().Add(5 * 24 * time.Hour)
	shortUntil := time.Now().Add(2 * time.Hour)
	a.Cool(GroupGemini, longUntil)
	a.Cool(GroupClaudeGPT, longUntil)

	// SyncCooldowns clears groups with quota available and shortens groups whose
	// only remaining exhaustion is a shorter window (e.g. weekly reset, 5h at 0).
	asOf := time.Now()
	a.SyncCooldowns(map[string]time.Time{GroupGemini: shortUntil}, asOf)
	st := a.Status()
	if got := st.Cooldowns[GroupGemini]; !got.Equal(shortUntil) {
		t.Fatalf("gemini cooldown = %v, want %v", got, shortUntil)
	}
	if _, ok := st.Cooldowns[GroupClaudeGPT]; ok {
		t.Fatalf("claude-gpt cooldown not cleared: %v", st.Cooldowns)
	}

	// A brand-new cooldown recorded after asOf (while the quota RPC was in
	// flight) is kept when SyncCooldowns omits it, unless the sample extends it.
	inFlightStart := time.Now()
	time.Sleep(2 * time.Millisecond)
	freshUntil := time.Now().Add(3 * time.Hour)
	a.Cool(GroupClaudeGPT, freshUntil)

	a.SyncCooldowns(map[string]time.Time{}, inFlightStart)
	st = a.Status()
	if _, ok := st.Cooldowns[GroupGemini]; ok {
		t.Fatalf("older gemini cooldown not cleared: %v", st.Cooldowns)
	}
	if got := st.Cooldowns[GroupClaudeGPT]; !got.Equal(freshUntil) {
		t.Fatalf("in-flight claude-gpt cooldown lost: got %v, want %v", got, freshUntil)
	}

	// Extending an in-flight cooldown still takes the later reset time.
	laterUntil := freshUntil.Add(time.Hour)
	a.SetCooldown(GroupClaudeGPT, laterUntil, inFlightStart)
	if got := a.Status().Cooldowns[GroupClaudeGPT]; !got.Equal(laterUntil) {
		t.Fatalf("extended cooldown = %v, want %v", got, laterUntil)
	}

	// Re-writing the same deadline keeps the original SetAt so debounce timers
	// do not reset on repeated exhausted refreshes.
	origSetAt := a.Cooldowns()[GroupClaudeGPT].SetAt
	if origSetAt.IsZero() {
		t.Fatal("Cooldowns() returned zero SetAt")
	}
	time.Sleep(2 * time.Millisecond)
	a.SetCooldown(GroupClaudeGPT, laterUntil, time.Now())
	if got := a.Cooldowns()[GroupClaudeGPT].SetAt; !got.Equal(origSetAt) {
		t.Fatalf("unchanged SetCooldown bumped SetAt: got %v, want %v", got, origSetAt)
	}

	// SetCooldown with zero clears the cooldown once asOf is past its setAt.
	a.SetCooldown(GroupClaudeGPT, time.Time{}, time.Now())
	if len(a.Status().Cooldowns) != 0 || len(a.Cooldowns()) != 0 {
		t.Fatalf("cooldowns after clear = %v, want empty", a.Status().Cooldowns)
	}
}

func TestProcCancelsWaitWithoutAbortingSharedStart(t *testing.T) {
	p, err := New(Config{
		Bin:             os.Args[0],
		TurnsPerAccount: 1,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	a, err := p.Add(AccountConfig{
		Name: "a",
		Home: filepath.Join(t.TempDir(), "a"),
		Env:  []string{agytest.EnvRun + "=1", "FAKE_AGY_INIT_DELAY=150ms"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := agy.Profile{Name: "agent-test"}

	// An already-cancelled ctx must return immediately without starting agy.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := a.Proc(cancelledCtx, profile); !errors.Is(err, context.Canceled) {
		t.Fatalf("Proc(cancelled) err = %v, want context.Canceled", err)
	}
	if len(a.LiveProcs()) != 0 {
		t.Fatalf("cancelled Proc started a process: %v", a.LiveProcs())
	}

	// Caller 1 starts the shared proc and cancels early; caller 2 waits on the
	// same start and must still receive the live process.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel1()

	type res struct {
		proc    *agy.Proc
		err     error
		elapsed time.Duration
	}
	ch1 := make(chan res, 1)
	go func() {
		start := time.Now()
		proc, err := a.Proc(ctx1, profile)
		ch1 <- res{proc, err, time.Since(start)}
	}()

	time.Sleep(10 * time.Millisecond)
	ch2 := make(chan res, 1)
	go func() {
		start := time.Now()
		proc, err := a.Proc(context.Background(), profile)
		ch2 <- res{proc, err, time.Since(start)}
	}()

	r1 := <-ch1
	if !errors.Is(r1.err, context.DeadlineExceeded) {
		t.Fatalf("caller 1 err = %v, want DeadlineExceeded", r1.err)
	}
	if r1.elapsed > 120*time.Millisecond {
		t.Fatalf("caller 1 waited %v for slow init instead of cancelling", r1.elapsed)
	}

	r2 := <-ch2
	if r2.err != nil || r2.proc == nil || !r2.proc.Alive() {
		t.Fatalf("caller 2 = (%v, %v), want live shared proc", r2.proc, r2.err)
	}
	t.Cleanup(func() {
		r2.proc.Kill()
		<-r2.proc.Done()
	})
}
