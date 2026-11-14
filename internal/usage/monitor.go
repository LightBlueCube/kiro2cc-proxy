// Package usage watches account quota and records requests: tokens,
// list-price cost, and the quota share each request used.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
)

// sampleEvery is how often accounts with a running agy are sampled.
// Sampling never starts agy; Refresh does.
const sampleEvery = 5 * time.Minute

const sampleTimeout = 30 * time.Second

// AccountQuota is an account's latest sample.
type AccountQuota struct {
	At     time.Time
	User   agy.UserStatus
	Groups []agy.QuotaGroup
	// Err is the last sampling failure, cleared by a success.
	Err   string
	ErrAt time.Time
}

type MonitorConfig struct {
	Store *store.Store
	Pool  *pool.Pool
	// Profile is the agent profile Refresh starts agy with.
	Profile agy.Profile
	// OnSample runs after each stored sample (quota attribution);
	// oldestRun is when the account's longest running turn began, zero
	// when none ran as sampling began. A parked turn is not running: its
	// model calls are recorded already.
	OnSample func(ctx context.Context, account string, at time.Time, oldestRun time.Time)
	Log      *slog.Logger
}

type Monitor struct {
	cfg MonitorConfig

	refreshMu sync.Mutex // one Refresh at a time
	// sampleMu orders samples and their attribution: concurrent ones
	// would settle the same drop twice.
	sampleMu sync.Mutex

	mu     sync.Mutex
	latest map[string]AccountQuota
	// settles holds by account the timer of SettleSoon.
	settles map[string]*time.Timer
}

func NewMonitor(cfg MonitorConfig) *Monitor {
	return &Monitor{cfg: cfg, latest: map[string]AccountQuota{}, settles: map[string]*time.Timer{}}
}

// Run samples every account with a running agy until ctx ends, and
// retires stale pending requests that never saw a quota drop.
func (m *Monitor) Run(ctx context.Context) {
	m.expirePending(ctx)
	ticker := time.NewTicker(sampleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.expirePending(ctx)
		for _, account := range m.cfg.Pool.Accounts() {
			m.SampleRunning(ctx, account)
		}
	}
}

// SampleRunning samples the account if its agy runs a language server;
// it never starts agy.
func (m *Monitor) SampleRunning(ctx context.Context, account *pool.Account) {
	proc, err := withLanguageServer(account.LiveProcs())
	if err != nil {
		m.fail(account.Name(), err)
		return
	}
	if proc == nil {
		return // no agy, or its idle sessions were all closed
	}
	m.sample(ctx, account, proc)
}

// maxQuotaHold bounds how long KeepForQuota keeps an idle account's agy
// running: a drop may never show (a bucket at 0, a use too small to move
// the fraction).
const maxQuotaHold = 15 * time.Minute

// pendingMaxAge is how long an unsettled request waits for a quota drop
// before being marked failed so stale rows do not stay pending forever.
const pendingMaxAge = time.Hour

func (m *Monitor) expirePending(ctx context.Context) {
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()
	m.expirePendingLocked(ctx)
}

func (m *Monitor) expirePendingLocked(ctx context.Context) {
	n, err := m.cfg.Store.ExpirePendingQuota(ctx, time.Now().Add(-pendingMaxAge), store.SkipFailed)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Warn("expiring stale pending quota failed", "err", err)
		}
		return
	}
	if n > 0 {
		m.cfg.Log.Info("marked stale pending quota as failed", "count", n)
	}
}

// KeepForQuota reports whether the account's last open session, idle
// since lastUsed, should stay open so a running agy can sample the quota:
// until SettleTime passed, and then while the account has unsettled
// requests, up to maxQuotaHold. Past SettleTime it samples the account.
func (m *Monitor) KeepForQuota(ctx context.Context, account *pool.Account, lastUsed time.Time) bool {
	idle := time.Since(lastUsed)
	if idle < SettleTime {
		return true
	}
	m.SampleRunning(ctx, account)
	if idle > maxQuotaHold {
		return false
	}
	unsettled, err := m.cfg.Store.HasUnsettledQuota(ctx, account.Name())
	if err != nil {
		m.cfg.Log.Error("checking for unsettled quota failed", "account", account.Name(), "err", err)
		return false
	}
	return unsettled
}

// settleDelay leaves the attributor's SettleTime after a request plus a
// margin for the record to be stored. A variable for tests.
var settleDelay = SettleTime + 10*time.Second

// SettleSoon samples the account settleDelay after its latest call, so
// the quota use of a burst of requests is settled once it shows.
func (m *Monitor) SettleSoon(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer := m.settles[name]; timer != nil {
		timer.Reset(settleDelay)
		return
	}
	m.settles[name] = time.AfterFunc(settleDelay, func() {
		if account := m.cfg.Pool.Account(name); account != nil {
			m.SampleRunning(context.Background(), account)
		}
	})
}

// withLanguageServer is the first process whose language server runs, or
// nil.
func withLanguageServer(procs []*agy.Proc) (*agy.Proc, error) {
	for _, proc := range procs {
		has, err := proc.HasLanguageServer()
		if err != nil {
			return nil, err
		}
		if has {
			return proc, nil
		}
	}
	return nil, nil
}

// Refresh samples the account now, starting agy and its language server
// when needed.
func (m *Monitor) Refresh(ctx context.Context, account *pool.Account) (AccountQuota, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	proc, err := account.Proc(ctx, m.cfg.Profile)
	probe := ""
	var models []agy.ModelInfo
	if err == nil {
		probe, models, err = proc.EnsureLanguageServer(ctx, agy.SessionSpec{Cwd: account.Workspace()})
	}
	if probe != "" {
		account.SetModels(models)
		defer m.closeProbe(account.Name(), proc, probe)
	}
	if err != nil {
		m.fail(account.Name(), err)
		q, _ := m.Latest(account.Name())
		return q, err
	}
	return m.sample(ctx, account, proc)
}

// closeProbe closes the empty session Refresh opened to start a language
// server.
func (m *Monitor) closeProbe(account string, proc *agy.Proc, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), sampleTimeout)
	defer cancel()
	if err := proc.CloseSession(ctx, sessionID); err != nil {
		m.cfg.Log.Warn("closing the quota probe session failed", "account", account, "session", sessionID, "err", err)
	}
}

func (m *Monitor) sample(ctx context.Context, account *pool.Account, proc *agy.Proc) (AccountQuota, error) {
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	name := account.Name()
	oldestRun, _ := account.OldestRun()
	user, err := proc.UserStatus(ctx)
	var groups []agy.QuotaGroup
	if err == nil {
		groups, err = proc.Quota(ctx)
	}
	if err != nil {
		m.fail(name, err)
		q, _ := m.Latest(name)
		return q, err
	}
	q := AccountQuota{At: time.Now(), User: user, Groups: groups}
	m.mu.Lock()
	m.latest[name] = q
	m.mu.Unlock()

	var samples []store.QuotaSample
	for _, g := range groups {
		for _, b := range g.Buckets {
			if group := BucketGroup(b.ID); group != "" && b.Remaining <= 0 && b.ResetAt.After(q.At) {
				account.Cool(group, b.ResetAt)
			}
			samples = append(samples, store.QuotaSample{Account: name, At: q.At, Bucket: b.ID, Group: g.Name, Window: b.Window, Remaining: b.Remaining, ResetAt: b.ResetAt})
		}
	}
	if err := m.cfg.Store.AddQuotaSamples(ctx, samples); err != nil {
		m.cfg.Log.Error("quota samples not stored", "account", name, "err", err)
		return q, nil
	}
	m.expirePendingLocked(ctx)
	if m.cfg.OnSample != nil {
		m.cfg.OnSample(ctx, name, q.At, oldestRun)
	}
	return q, nil
}

func (m *Monitor) fail(name string, err error) {
	m.cfg.Log.Warn("quota sample failed", "account", name, "err", err)
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.latest[name]
	q.Err, q.ErrAt = err.Error(), time.Now()
	m.latest[name] = q
}

// Latest is the account's newest sample, if any.
func (m *Monitor) Latest(name string) (AccountQuota, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.latest[name]
	return q, ok
}

// Forget drops a removed account's sample and pending settle.
func (m *Monitor) Forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.latest, name)
	if timer := m.settles[name]; timer != nil {
		timer.Stop()
		delete(m.settles, name)
	}
}
