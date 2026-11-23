package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/cloudcode"
	"agyrelay/internal/conv"
	"agyrelay/internal/egress"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/promptflags"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWeeklyBucketNeedsWarmup(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		bucket agy.QuotaBucket
		want   bool
	}{
		{
			name:   "below 100%",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 0.999, ResetAt: now.Add(weeklyWindow)},
			want:   false,
		},
		{
			name:   "full with zero reset time",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 1.0},
			want:   true,
		},
		{
			name:   "full with rolling reset at +7d",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 1.0, ResetAt: now.Add(weeklyWindow)},
			want:   true,
		},
		{
			name:   "full within rolling grace (+7d - 10m)",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 1.0, ResetAt: now.Add(weeklyWindow - 10*time.Minute)},
			want:   true,
		},
		{
			name:   "full but countdown already fixed (+7d - 30m)",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 1.0, ResetAt: now.Add(weeklyWindow - 30*time.Minute)},
			want:   false,
		},
		{
			name:   "full with expired reset time",
			bucket: agy.QuotaBucket{ID: "gemini-weekly", Window: "weekly", Remaining: 1.0, ResetAt: now.Add(-time.Minute)},
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := weeklyBucketNeedsWarmup(tc.bucket, now); got != tc.want {
				t.Fatalf("weeklyBucketNeedsWarmup = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickWarmupModel(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pool.New(pool.Config{TurnsPerAccount: 1, Log: log, Accounts: []pool.AccountConfig{
		{ID: "pro", Name: "pro", Home: filepath.Join(t.TempDir(), "pro")},
		{ID: "ultra", Name: "ultra", Home: filepath.Join(t.TempDir(), "ultra")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pro := p.Account("pro")
	// Restrict routing to a single model to verify warmup ignores AllowedModels.
	pro.SetRouting(0, []string{"claude-opus-5-5"})
	pro.SetQuotaGroups([]string{pool.GroupGemini, pool.GroupClaudeGPT, pool.GroupArgon})

	catalog := []conv.ModelInfo{
		{ID: "gemini-4-neon", Efforts: []string{"low", "high"}, Slugs: []string{"gemini-4-neon-low", "gemini-4-neon-high"}},
		{ID: "gemini-3.8-flash", Efforts: []string{"low", "medium"}, Slugs: []string{"gemini-3.8-flash-low", "gemini-3.8-flash-medium"}},
		{ID: "gemini-4-argon", Efforts: []string{"low", "high"}, Slugs: []string{"gemini-4-argon-low", "gemini-4-argon-high"}},
		{ID: "claude-opus-5-5", Efforts: []string{"low", "high"}, Slugs: []string{"claude-opus-5-5-low", "claude-opus-5-5-high"}},
		{ID: "claude-sonnet-5-5", Efforts: []string{"low", "high"}, Slugs: []string{"claude-sonnet-5-5-low", "claude-sonnet-5-5-high"}},
		{ID: "gpt-oss-120b", Efforts: []string{"medium"}, Slugs: []string{"gpt-oss-120b-medium"}},
		{ID: "claude-unpriced", Efforts: []string{"low"}, Slugs: []string{"claude-unpriced-low"}},
	}

	// Default picks cheapest priced model in each pool (lowest effort tier).
	if got, missing, ok := pickWarmupModel(pro, catalog, pool.GroupGemini, "", usage.DefaultPrices); !ok || missing || got.slug != "gemini-3.8-flash-low" || got.effort != "low" {
		t.Fatalf("gemini default = %+v, missing=%v, ok=%v; want gemini-3.8-flash-low", got, missing, ok)
	}
	if got, missing, ok := pickWarmupModel(pro, catalog, pool.GroupClaudeGPT, "", usage.DefaultPrices); !ok || missing || got.slug != "gpt-oss-120b-medium" || got.model != "gpt-oss-120b" {
		t.Fatalf("claude-gpt default (with oss) = %+v, missing=%v, ok=%v; want gpt-oss-120b-medium", got, missing, ok)
	}
	// When gpt-oss-120b leaves the catalog, cheapest priced Claude wins over unpriced.
	noOSS := append([]conv.ModelInfo{}, catalog[:5]...)
	noOSS = append(noOSS, catalog[6])
	if got, missing, ok := pickWarmupModel(pro, noOSS, pool.GroupClaudeGPT, "", usage.DefaultPrices); !ok || missing || got.slug != "claude-sonnet-5-5-low" {
		t.Fatalf("claude-gpt default (without oss) = %+v, missing=%v, ok=%v; want claude-sonnet-5-5-low", got, missing, ok)
	}
	if got, missing, ok := pickWarmupModel(pro, catalog, pool.GroupArgon, "", usage.DefaultPrices); !ok || missing || got.slug != "gemini-4-argon-low" {
		t.Fatalf("argon default = %+v, missing=%v, ok=%v; want gemini-4-argon-low", got, missing, ok)
	}

	// Explicit override by base model id or exact tier slug.
	if got, missing, ok := pickWarmupModel(pro, catalog, pool.GroupClaudeGPT, "claude-opus-5-5", usage.DefaultPrices); !ok || missing || got.slug != "claude-opus-5-5-low" {
		t.Fatalf("configured base = %+v, missing=%v, ok=%v; want claude-opus-5-5-low", got, missing, ok)
	}
	if got, missing, ok := pickWarmupModel(pro, catalog, pool.GroupClaudeGPT, "claude-opus-5-5-high", usage.DefaultPrices); !ok || missing || got.slug != "claude-opus-5-5-high" || got.effort != "high" {
		t.Fatalf("configured slug = %+v, missing=%v, ok=%v; want claude-opus-5-5-high", got, missing, ok)
	}

	// Missing configured model falls back to cheapest and reports configuredMissing.
	if got, missing, ok := pickWarmupModel(pro, noOSS, pool.GroupClaudeGPT, "gpt-oss-120b", usage.DefaultPrices); !ok || !missing || got.slug != "claude-sonnet-5-5-low" {
		t.Fatalf("missing configured = %+v, missing=%v, ok=%v; want fallback claude-sonnet-5-5-low with missing=true", got, missing, ok)
	}

	// Custom price list controls cheapest selection.
	customPrices := []usage.Price{
		{Model: "claude-opus-5-5", Input: 0.5, Output: 1},
		{Model: "claude-sonnet-5-5", Input: 2, Output: 10},
	}
	if got, _, ok := pickWarmupModel(pro, noOSS, pool.GroupClaudeGPT, "", customPrices); !ok || got.model != "claude-opus-5-5" {
		t.Fatalf("custom prices = %+v, ok=%v; want claude-opus-5-5", got, ok)
	}

	// Ultra account merges carbon into gemini when argon bucket is absent.
	ultra := p.Account("ultra")
	ultra.SetModels([]agy.ModelInfo{{ModelID: "gemini-4-carbon-low"}})
	ultra.SetQuotaGroups([]string{pool.GroupGemini, pool.GroupClaudeGPT})
	ultraCatalog := []conv.ModelInfo{{ID: "gemini-4-carbon", Efforts: []string{"low"}, Slugs: []string{"gemini-4-carbon-low"}}}
	if got, _, ok := pickWarmupModel(ultra, ultraCatalog, pool.GroupGemini, "", usage.DefaultPrices); !ok || got.slug != "gemini-4-carbon-low" {
		t.Fatalf("ultra gemini = %+v, ok=%v; want gemini-4-carbon-low", got, ok)
	}
	if _, _, ok := pickWarmupModel(ultra, ultraCatalog, pool.GroupArgon, "", usage.DefaultPrices); ok {
		t.Fatal("ultra argon unexpectedly matched gemini-4-carbon")
	}
}

func TestValidateWarmupModels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.SetSetting(ctx, settingsKey, map[string]any{
		"warmup_full_pools": true,
		"warmup_models": map[string]string{
			"gemini":     " gemini-3.8-flash ",
			"claude-gpt": "claude-sonnet-5-5-low",
			"argon":      "  ",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !s.WarmupFullPools {
		t.Fatal("WarmupFullPools = false, want true")
	}
	if s.WarmupModels[pool.GroupGemini] != "gemini-3.8-flash" || s.WarmupModels[pool.GroupClaudeGPT] != "claude-sonnet-5-5-low" {
		t.Fatalf("WarmupModels = %+v", s.WarmupModels)
	}
	if _, ok := s.WarmupModels[pool.GroupArgon]; ok {
		t.Fatalf("blank argon override was not dropped: %+v", s.WarmupModels)
	}

	for _, bad := range []map[string]string{
		{"unknown": "gemini-3.8-flash"},
		{pool.GroupGemini: "claude-opus-5-5"},
		{pool.GroupClaudeGPT: "gemini-3.8-flash"},
		{pool.GroupArgon: "gemini-3.8-flash"},
		{pool.GroupGemini: "bad model"},
	} {
		cfg := DefaultSettings()
		cfg.WarmupModels = bad
		if err := cfg.validate(); err == nil {
			t.Errorf("invalid warmup_models %+v passed validation", bad)
		}
	}
}

type warmupEnv struct {
	st      *store.Store
	pool    *pool.Pool
	monitor *usage.Monitor
	gw      *gateway.Gateway
	ctl     *Control
}

func newWarmupEnv(t *testing.T) *warmupEnv {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(dir, "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	b, err := bridge.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })

	p, err := pool.New(pool.Config{Bin: os.Args[0], TurnsPerAccount: 4, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	monitor := usage.NewMonitor(usage.MonitorConfig{
		Store:    st,
		Pool:     p,
		Profile:  gateway.IdleProfile(),
		OnSample: usage.NewAttributor(st, log).Attribute,
		Log:      log,
	})
	settings, err := LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	settings.AllowDirect = true
	recorder := usage.NewRecorder(st, settings.Prices, nil, log)
	budget := usage.NewBudget(st, recorder)
	flags, err := promptflags.New(context.Background(), st, log)
	if err != nil {
		t.Fatal(err)
	}
	eg, err := egress.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eg.Close() })
	cc, err := cloudcode.Start(cloudcode.DefaultUpstream, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })

	gw, err := gateway.New(gateway.Config{
		Pool:        p,
		Bridge:      b,
		DataDir:     dir,
		SnapshotTTL: time.Hour,
		IdleClose:   time.Hour,
		Recorder:    recorder,
		Budget:      budget,
		Flags:       flags,
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)

	ctl, err := New(context.Background(), Config{
		Store:       st,
		Pool:        p,
		Gateway:     gw,
		Monitor:     monitor,
		Recorder:    recorder,
		Budget:      budget,
		Flags:       flags,
		Egress:      eg,
		CloudCode:   cc,
		AgyBin:      os.Args[0],
		Level:       new(slog.LevelVar),
		AccountsDir: filepath.Join(dir, "accounts"),
		Settings:    settings,
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &warmupEnv{st: st, pool: p, monitor: monitor, gw: gw, ctl: ctl}
}

func TestWarmupPassEndToEnd(t *testing.T) {
	e := newWarmupEnv(t)
	ctx := context.Background()

	acct, err := e.ctl.AddAccount(ctx, "main", []string{
		agytest.EnvRun + "=1",
		"FAKE_AGY_QUOTA_RESET_HOURS=168",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Restrict routing to gemini only; warmup must still warm claude-gpt too.
	if err := e.ctl.SetAccountRouting(ctx, acct.ID(), 0, []string{"gemini-3.8-flash"}); err != nil {
		t.Fatal(err)
	}

	// Disabled by default: warmupPass does nothing and spawns no agy process.
	if next := e.ctl.warmupPass(ctx, time.Now()); !next.IsZero() {
		t.Fatalf("disabled warmupPass returned next=%v, want zero", next)
	}
	if len(acct.LiveProcs()) != 0 {
		t.Fatalf("disabled warmup started %d procs", len(acct.LiveProcs()))
	}

	// Enable warmup: an un-refreshed account with no quota samples is never probed by warmup.
	s := e.ctl.Settings()
	s.WarmupFullPools = true
	if err := e.ctl.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if next := e.ctl.warmupPass(ctx, time.Now()); next.IsZero() {
		t.Fatal("enabled warmupPass returned zero next wake")
	}
	if len(acct.LiveProcs()) != 0 {
		t.Fatalf("warmup probed un-refreshed account: %d procs", len(acct.LiveProcs()))
	}

	// Refresh the account: warmup waits out the 30s post-refresh debounce before sending calls.
	if err := e.ctl.RefreshAccount(ctx, acct.ID()); err != nil {
		t.Fatal(err)
	}
	next := e.ctl.warmupPass(ctx, time.Now())
	if next.IsZero() {
		t.Fatal("enabled warmupPass returned zero next wake")
	}
	if reqs, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 20); err != nil || len(reqs) != 0 {
		t.Fatalf("requests before 30s debounce elapsed = %+v, %v; want none", reqs, err)
	}

	// Once the 30s post-refresh debounce elapses, warmup runs without re-refreshing.
	next = e.ctl.warmupPass(ctx, next)
	if next.IsZero() {
		t.Fatal("post-debounce warmupPass returned zero next wake")
	}

	reqs, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("recorded requests = %d, want 2: %+v", len(reqs), reqs)
	}
	groups := map[string]store.Request{}
	for _, r := range reqs {
		if r.KeyHash != store.SystemKeyWarmup || r.KeyName != warmupKeyName || r.API != "" || r.Mode != "" || r.Status != store.StatusOK {
			t.Errorf("unexpected warmup request row: %+v", r)
		}
		if r.Input+r.Output == 0 {
			t.Errorf("warmup request has zero tokens: %+v", r)
		}
		groups[r.QuotaGroup] = r
	}
	if groups[pool.GroupGemini].Slug != "gemini-3.8-flash-low" {
		t.Errorf("gemini warmup slug = %q, want gemini-3.8-flash-low", groups[pool.GroupGemini].Slug)
	}
	if groups[pool.GroupClaudeGPT].Slug != "claude-opus-5-5-high" {
		t.Errorf("claude-gpt warmup slug = %q, want claude-opus-5-5-high", groups[pool.GroupClaudeGPT].Slug)
	}

	// Synthetic warmup key must never populate key billing tables.
	if spends, err := e.st.KeySpends(ctx, ""); err != nil || len(spends) != 0 {
		t.Fatalf("key_usage = %+v, %v; want empty", spends, err)
	}
	if totals, err := e.st.KeyTotals(ctx, ""); err != nil || len(totals) != 0 {
		t.Fatalf("key_totals = %+v, %v; want empty", totals, err)
	}

	// A second immediate pass must not repeat warmup calls.
	e.ctl.warmupPass(ctx, next)
	reqs2, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs2) != 2 {
		t.Fatalf("second immediate pass created extra requests: got %d, want 2", len(reqs2))
	}

	// Once a post-warmup quota sample observes remaining < 1.0, group backoff clears.
	if _, err := e.monitor.Refresh(ctx, acct); err != nil {
		t.Fatal(err)
	}
	e.ctl.warmupPass(ctx, time.Now())
	e.ctl.warmup.mu.Lock()
	remainingGroups := len(e.ctl.warmup.accounts[acct.ID()].groups)
	e.ctl.warmup.mu.Unlock()
	if remainingGroups != 0 {
		t.Fatalf("warmup group states after drop = %d, want 0", remainingGroups)
	}
}

func TestWarmupSkipsStartupRefreshWhenStoredSampleAlreadyTicking(t *testing.T) {
	e := newWarmupEnv(t)
	ctx := context.Background()

	acct, err := e.ctl.AddAccount(ctx, "main", []string{
		agytest.EnvRun + "=1",
		"FAKE_AGY_QUOTA_RESET_HOURS=168",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.st.AddQuotaSamples(ctx, []store.QuotaSample{
		{Account: acct.ID(), At: now.Add(-10 * time.Minute), Bucket: "gemini-weekly", Group: "Gemini", Window: "weekly", Remaining: 0.85, ResetAt: now.Add(5 * 24 * time.Hour)},
		{Account: acct.ID(), At: now.Add(-10 * time.Minute), Bucket: "3p-weekly", Group: "Claude", Window: "weekly", Remaining: 0.90, ResetAt: now.Add(6 * 24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	s := e.ctl.Settings()
	s.WarmupFullPools = true
	if err := e.ctl.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	next := e.ctl.warmupPass(ctx, now)
	if next.IsZero() {
		t.Fatal("expected non-zero next wake")
	}
	if len(acct.LiveProcs()) != 0 {
		t.Fatalf("warmup started %d agy processes despite ticking stored quota samples", len(acct.LiveProcs()))
	}
	if reqs, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 10); err != nil || len(reqs) != 0 {
		t.Fatalf("requests = %+v, %v; want none", reqs, err)
	}
}

func TestWarmupSkipsPausedAndRecentGroupUsage(t *testing.T) {
	e := newWarmupEnv(t)
	ctx := context.Background()

	paused, err := e.ctl.AddAccount(ctx, "paused", []string{agytest.EnvRun + "=1", "FAKE_AGY_QUOTA_RESET_HOURS=168"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ctl.SetAccountPaused(ctx, paused.ID(), true); err != nil {
		t.Fatal(err)
	}

	active, err := e.ctl.AddAccount(ctx, "active", []string{agytest.EnvRun + "=1", "FAKE_AGY_QUOTA_RESET_HOURS=168"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ctl.RefreshAccount(ctx, active.ID()); err != nil {
		t.Fatal(err)
	}
	// Also refresh paused manually to confirm even a sampled paused account is never warmed.
	if err := e.ctl.RefreshAccount(ctx, paused.ID()); err != nil {
		t.Fatal(err)
	}
	// Record a recent user request in gemini so warmup skips gemini and only warms claude-gpt.
	now := time.Now()
	if err := e.st.AddRequest(ctx, store.Request{
		At: now.Add(-2 * time.Minute), Duration: time.Second,
		KeyHash: "userhash", KeyName: "alice",
		Account: active.ID(), AccountName: active.Name(),
		Model: "gemini-3.8-flash", Slug: "gemini-3.8-flash-low", Effort: "low",
		API: "chat", Mode: "fresh", Status: store.StatusOK, Finish: "stop",
		Input: 100, Output: 20, QuotaGroup: pool.GroupGemini, KeyGroup: pool.GroupGemini,
	}); err != nil {
		t.Fatal(err)
	}

	s := e.ctl.Settings()
	s.WarmupFullPools = true
	if err := e.ctl.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	next := e.ctl.warmupPass(ctx, now)
	e.ctl.warmupPass(ctx, next)

	reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: store.SystemKeyWarmup}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].Account != active.ID() || reqs[0].QuotaGroup != pool.GroupClaudeGPT {
		t.Fatalf("warmup requests = %+v, want only claude-gpt on active", reqs)
	}
}

func TestWarmupDebounceSkipsPausedUncheckedAndOutOfRotationAccounts(t *testing.T) {
	e := newWarmupEnv(t)
	ctx := context.Background()

	addFull := func(name, proxy string) *pool.Account {
		t.Helper()
		a, err := e.ctl.AddAccount(ctx, name, []string{agytest.EnvRun + "=1", "FAKE_AGY_QUOTA_RESET_HOURS=168"}, proxy)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	const acctProxy = "http://127.0.0.1:7890"
	unchecked := addFull("unchecked", acctProxy)
	pausedInWindow := addFull("paused-in-window", acctProxy)
	banned := addFull("banned", acctProxy)
	disabled := addFull("disabled", acctProxy)
	verify := addFull("verify", acctProxy)
	noProxy := addFull("noproxy", "")

	for _, a := range []*pool.Account{pausedInWindow, banned, verify} {
		if err := e.ctl.RefreshAccount(ctx, a.ID()); err != nil {
			t.Fatal(err)
		}
	}
	disabled.Disable(errors.New("session/new: Authentication required"))

	s := e.ctl.Settings()
	s.AllowDirect = false
	s.WarmupFullPools = true
	if err := e.ctl.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}

	// Pass 1 enters the 30s debounce window for refreshed accounts and leaves unchecked/out-of-rotation untouched.
	due := e.ctl.warmupPass(ctx, time.Now())
	if reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: store.SystemKeyWarmup}, 0, 20); err != nil || len(reqs) != 0 {
		t.Fatalf("warmup sent during debounce window: %+v, %v", reqs, err)
	}
	for _, untouched := range []*pool.Account{unchecked, disabled, noProxy} {
		if n := len(untouched.LiveProcs()); n != 0 {
			t.Errorf("account %s spawned %d agy procs", untouched.Name(), n)
		}
	}

	// During the 30s window after refresh, accounts are paused or leave rotation.
	if err := e.ctl.SetAccountPaused(ctx, pausedInWindow.ID(), true); err != nil {
		t.Fatal(err)
	}
	e.ctl.pauseAccountForBan(banned.ID(), "PERMISSION_DENIED (code 403): TOS_VIOLATION")
	verify.SetVerification(true, "https://accounts.google.com/v")

	// Pass 2 after the 30s debounce must skip all of them (and still not probe unchecked).
	e.ctl.warmupPass(ctx, due)
	if n := len(unchecked.LiveProcs()); n != 0 {
		t.Fatalf("unchecked account spawned %d agy procs after debounce", n)
	}
	if reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: store.SystemKeyWarmup}, 0, 20); err != nil || len(reqs) != 0 {
		t.Fatalf("warmup requests on paused/out-of-rotation accounts = %+v, %v; want none", reqs, err)
	}

	// Resuming returns the account to rotation; warmup now proceeds since 30s already elapsed.
	if err := e.ctl.SetAccountPaused(ctx, pausedInWindow.ID(), false); err != nil {
		t.Fatal(err)
	}
	e.ctl.warmupPass(ctx, due)
	reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: store.SystemKeyWarmup}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("warmup requests after resume = %d (%+v), want 2", len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.Account != pausedInWindow.ID() {
			t.Errorf("warmed unexpected account %s (%s)", r.AccountName, r.Account)
		}
	}
}
