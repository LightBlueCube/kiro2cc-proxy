package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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
