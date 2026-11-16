package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// monitorEnv is a Monitor over one fake agy account "a"; quotaReads counts
// the quota reads its agy served.
type monitorEnv struct {
	st         *store.Store
	p          *pool.Pool
	monitor    *Monitor
	quotaReads func() int
}

func newMonitorEnv(t *testing.T) monitorEnv {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "acp.jsonl")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := pool.New(pool.Config{Bin: os.Args[0], TurnsPerAccount: 1, Log: log, Accounts: []pool.AccountConfig{{
		Name: "a", Home: filepath.Join(dir, "home"), Env: []string{agytest.EnvRun + "=1", "FAKE_AGY_LOG=" + logPath},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	monitor := NewMonitor(MonitorConfig{Store: st, Pool: p, Profile: gateway.IdleProfile(), OnSample: NewAttributor(st, log).Attribute, Log: log})
	quotaReads := func() int {
		t.Helper()
		f, err := os.Open(logPath)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		n := 0
		for scanner := bufio.NewScanner(f); scanner.Scan(); {
			var line struct{ Method string }
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				t.Fatal(err)
			}
			if line.Method == "ls/RetrieveUserQuotaSummary" {
				n++
			}
		}
		return n
	}
	return monitorEnv{st: st, p: p, monitor: monitor, quotaReads: quotaReads}
}

// SettleSoon samples once after a burst of requests, never starting agy.
func TestSettleSoon(t *testing.T) {
	e := newMonitorEnv(t)
	monitor, p, quotaReads := e.monitor, e.p, e.quotaReads
	defer func(d time.Duration) { settleDelay = d }(settleDelay)
	settleDelay = 50 * time.Millisecond

	// Without a running agy there is nothing to sample.
	monitor.SettleSoon("a")
	time.Sleep(200 * time.Millisecond)
	if n, procs := quotaReads(), len(p.Account("a").LiveProcs()); n != 0 || procs != 0 {
		t.Fatalf("without agy: %d quota reads, %d processes", n, procs)
	}

	if _, err := monitor.Refresh(context.Background(), p.Account("a")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		monitor.SettleSoon("a")
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := quotaReads(); n != 2 { // Refresh's and one settle
		t.Fatalf("quota reads = %d, want 2", n)
	}

	// A removed account's settle is dropped.
	monitor.SettleSoon("a")
	monitor.Forget("a")
	time.Sleep(200 * time.Millisecond)
	if n := quotaReads(); n != 2 {
		t.Fatalf("after Forget: quota reads = %d, want 2", n)
	}
}

// A refresh that agy answers with "Authentication required" takes the
// account out of rotation, as a request's does, until it signs in again.
func TestRefreshDisablesAccountThatNeedsSignIn(t *testing.T) {
	e := newMonitorEnv(t)
	ctx := context.Background()
	account := e.p.Account("a")
	tokenDir := filepath.Join(account.GeminiDir(), "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(tokenDir, "antigravity-oauth-token")
	if err := os.WriteFile(tokenFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := e.monitor.Refresh(ctx, account)
	if !agy.IsAuthError(err) {
		t.Fatalf("Refresh error = %v, want a sign-in error", err)
	}
	if got := account.Status().Disabled; got == "" {
		t.Fatal("account stays in rotation after its refresh needed a sign-in")
	}
	if err := account.Available("gemini"); err == nil {
		t.Fatal("a disabled account is available")
	}

	signedIn := `{"token": {"refresh_token": "` + agytest.RefreshToken + `"}, "auth_method": "consumer"}`
	if err := os.WriteFile(tokenFile, []byte(signedIn), 0o600); err != nil {
		t.Fatal(err)
	}
	account.SignedIn()
	if _, err := e.monitor.Refresh(ctx, account); err != nil {
		t.Fatalf("Refresh after signing in: %v", err)
	}
	if got := account.Status().Disabled; got != "" {
		t.Fatalf("account still disabled after signing in: %q", got)
	}
}

// When a previously refused sign-in works again (e.g. after a transient
// network error), Refresh probes session/new even if a language server
// already runs, re-enabling the account without killing live processes.
// SampleRunning also re-enables a disabled account on a successful sample.
func TestRefreshAndSampleEnableRecoveredAccount(t *testing.T) {
	e := newMonitorEnv(t)
	ctx := context.Background()
	account := e.p.Account("a")
	tokenDir := filepath.Join(account.GeminiDir(), "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(tokenDir, "antigravity-oauth-token")
	valid := `{"token": {"refresh_token": "` + agytest.RefreshToken + `"}, "auth_method": "consumer"}`
	if err := os.WriteFile(tokenFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}

	// Open a live session so a language server already runs on IdleProfile.
	proc, err := account.Proc(ctx, e.monitor.cfg.Profile)
	if err != nil {
		t.Fatal(err)
	}
	sessID, _, err := proc.NewSession(ctx, agy.SessionSpec{Cwd: account.Workspace()})
	if err != nil {
		t.Fatal(err)
	}

	// While the token is still rejected, Refresh must probe session/new and
	// keep the account disabled despite the already-running language server.
	if err := os.WriteFile(tokenFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	account.Disable(agy.ErrAuthRequired)
	if _, err := e.monitor.Refresh(ctx, account); !agy.IsAuthError(err) {
		t.Fatalf("Refresh with invalid token = %v, want auth error", err)
	}
	if account.Status().Disabled == "" {
		t.Fatal("account re-enabled while session/new still fails")
	}

	// Once the sign-in works again, Refresh re-enables without calling SignedIn
	// and without killing the running process or its live session.
	if err := os.WriteFile(tokenFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := e.monitor.Refresh(ctx, account)
	if err != nil {
		t.Fatalf("Refresh after recovery: %v", err)
	}
	if q.Err != "" || len(q.Groups) == 0 {
		t.Fatalf("quota after Refresh = %+v", q)
	}
	if got := account.Status().Disabled; got != "" {
		t.Fatalf("account still disabled after Refresh succeeded: %q", got)
	}
	if account.Available("gemini") != nil {
		t.Fatalf("account still unavailable: %v", account.Available("gemini"))
	}
	if !proc.Alive() || !proc.HasSession(sessID) {
		t.Fatal("Refresh killed existing live process or session")
	}

	// SampleRunning also re-enables a disabled account when sampling succeeds.
	account.Disable(agy.ErrAuthRequired)
	e.monitor.SampleRunning(ctx, account)
	if got := account.Status().Disabled; got != "" {
		t.Fatalf("account still disabled after SampleRunning succeeded: %q", got)
	}

	// Non-auth refresh failures (e.g. cancellation) leave a disabled account
	// disabled when it does not await verification.
	account.Disable(agy.ErrAuthRequired)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.monitor.Refresh(cancelled, account); err == nil {
		t.Fatal("Refresh with cancelled context succeeded")
	}
	if account.Status().Disabled == "" {
		t.Fatal("account re-enabled after failed Refresh")
	}

	// If Refresh fails before session/new completes (e.g. cancelled), a
	// disabled account stays disabled even when Verify is set. If auth
	// succeeded and sampling failed due to VALIDATION_REQUIRED, the stale
	// sign-in disable clears so verification is shown instead.
	account.SetVerification(true, "https://accounts.google.com/v")
	if err := os.WriteFile(tokenFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.monitor.Refresh(ctx, account); !agy.IsAuthError(err) {
		t.Fatalf("Refresh auth failure = %v", err)
	}
	if account.Status().Disabled == "" {
		t.Fatal("auth failure must stay disabled even if Verify was set earlier")
	}
	if _, err := e.monitor.Refresh(cancelled, account); err == nil {
		t.Fatal("Refresh with cancelled context succeeded")
	}
	if account.Status().Disabled == "" {
		t.Fatal("cancelled Refresh cleared disabled when Verify was set")
	}
	finishRefresh(account, true, errors.New("verify your account to continue"))
	if account.Status().Disabled != "" {
		t.Fatalf("disabled = %q, want empty once auth succeeded and Verify is set", account.Status().Disabled)
	}
	account.SetVerification(false, "")
	if err := os.WriteFile(tokenFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.monitor.Refresh(ctx, account); err != nil {
		t.Fatalf("Refresh with valid token: %v", err)
	}
	if account.Status().Disabled != "" {
		t.Fatalf("disabled = %q, want empty", account.Status().Disabled)
	}
}

// A sample passes on when the account's longest running turn began (zero
// when none runs), not counting turns that only wait for tool results.
func TestSampleReportsOldestRun(t *testing.T) {
	e := newMonitorEnv(t)
	ctx := context.Background()
	account := e.p.Account("a")
	var runs []time.Time
	monitor := NewMonitor(MonitorConfig{
		Store: e.st, Pool: e.p, Profile: gateway.IdleProfile(),
		OnSample: func(_ context.Context, _ string, _ time.Time, since time.Time) { runs = append(runs, since) },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	sample := func() {
		t.Helper()
		if _, err := monitor.Refresh(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	early := time.Now().Add(-10 * time.Minute)
	late := early.Add(5 * time.Minute)
	turn, other := new(int), new(int)

	sample() // no turn
	if err := account.TryAcquire("gemini"); err != nil {
		t.Fatal(err)
	}
	account.BeginRun(turn, early)
	sample() // running
	account.EndRun(turn)
	sample() // parked
	account.BeginRun(turn, late)
	account.BeginRun(other, early)
	sample() // two run: the older counts
	account.EndRun(other)
	account.EndRun(turn)
	account.Release()
	sample() // ended

	want := []time.Time{{}, early, {}, early, {}}
	if len(runs) != len(want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	for i := range want {
		if !runs[i].Equal(want[i]) {
			t.Fatalf("runs = %v, want %v", runs, want)
		}
	}
}

// KeepForQuota keeps an idle account's last session through SettleTime,
// then, sampling, while requests wait for their quota, up to maxQuotaHold.
func TestKeepForQuota(t *testing.T) {
	e := newMonitorEnv(t)
	ctx := context.Background()
	account := e.p.Account("a")
	if _, err := e.monitor.Refresh(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddRequest(ctx, store.Request{At: time.Now(), Account: "a", Status: store.StatusOK, QuotaGroup: "gemini", Input: 100}); err != nil {
		t.Fatal(err)
	}
	reads := e.quotaReads()

	if !e.monitor.KeepForQuota(ctx, account, time.Now()) || e.quotaReads() != reads {
		t.Fatal("within SettleTime: not kept, or sampled")
	}
	settled := time.Now().Add(-SettleTime - time.Minute)
	if !e.monitor.KeepForQuota(ctx, account, settled) || e.quotaReads() != reads+1 {
		t.Fatalf("with a request waiting: not kept, or %d quota reads, want %d", e.quotaReads(), reads+1)
	}
	if e.monitor.KeepForQuota(ctx, account, time.Now().Add(-maxQuotaHold-time.Minute)) {
		t.Fatal("kept past maxQuotaHold")
	}
	if err := e.st.SkipQuota(ctx, "a", "gemini", time.Now().Add(time.Minute), store.SkipReset); err != nil {
		t.Fatal(err)
	}
	if e.monitor.KeepForQuota(ctx, account, settled) {
		t.Fatal("kept with nothing waiting")
	}
}

func TestExpirePendingQuota(t *testing.T) {
	e := newMonitorEnv(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	if err := e.st.AddRequest(ctx, store.Request{
		At: now.Add(-pendingMaxAge - time.Minute), Account: "a", Status: store.StatusOK, QuotaGroup: "gemini", Input: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddRequest(ctx, store.Request{
		At: now.Add(-5 * time.Minute), Account: "a", Status: store.StatusOK, QuotaGroup: "gemini", Input: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// Run expires stale pending requests right on startup without starting agy.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.monitor.Run(runCtx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 2 && rows[1].QuotaSkip == store.SkipFailed {
			if rows[0].QuotaSkip != "" {
				t.Fatalf("recent request expired too: %+v", rows[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale request not expired: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}
