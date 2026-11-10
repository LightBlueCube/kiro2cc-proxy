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
	// OnSample runs after each stored sample (quota attribution).
	OnSample func(ctx context.Context, account string, at time.Time)
	Log      *slog.Logger
}

type Monitor struct {
	cfg MonitorConfig

	refreshMu sync.Mutex // one Refresh at a time

	mu     sync.Mutex
	latest map[string]AccountQuota
}

func NewMonitor(cfg MonitorConfig) *Monitor {
	return &Monitor{cfg: cfg, latest: map[string]AccountQuota{}}
}

// Run samples every account with a running agy until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(sampleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, account := range m.cfg.Pool.Accounts() {
			procs := account.LiveProcs()
			if len(procs) == 0 {
				continue
			}
			m.sample(ctx, account.Name(), procs[0])
		}
	}
}

// Refresh samples the account now, starting agy and its language server
// when needed.
func (m *Monitor) Refresh(ctx context.Context, account *pool.Account) (AccountQuota, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	proc, err := account.Proc(ctx, m.cfg.Profile)
	if err == nil {
		err = proc.EnsureLanguageServer(ctx, agy.SessionSpec{Cwd: account.Workspace()})
	}
	if err != nil {
		m.fail(account.Name(), err)
		q, _ := m.Latest(account.Name())
		return q, err
	}
	return m.sample(ctx, account.Name(), proc)
}

func (m *Monitor) sample(ctx context.Context, name string, proc *agy.Proc) (AccountQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
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
			samples = append(samples, store.QuotaSample{Account: name, At: q.At, Bucket: b.ID, Group: g.Name, Window: b.Window, Remaining: b.Remaining, ResetAt: b.ResetAt})
		}
	}
	if err := m.cfg.Store.AddQuotaSamples(ctx, samples); err != nil {
		m.cfg.Log.Error("quota samples not stored", "account", name, "err", err)
		return q, nil
	}
	if m.cfg.OnSample != nil {
		m.cfg.OnSample(ctx, name, q.At)
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

// Forget drops a removed account's sample.
func (m *Monitor) Forget(name string) {
	m.mu.Lock()
	delete(m.latest, name)
	m.mu.Unlock()
}
