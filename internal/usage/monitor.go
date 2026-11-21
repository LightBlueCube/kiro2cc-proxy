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
		m.fail(account, err)
		return
	}
	if proc == nil {
		return // no agy, or its idle sessions were all closed
	}
	if _, err := m.sample(ctx, account, proc, cooldownBackground); err == nil {
		account.Enable()
	}
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
	unsettled, err := m.cfg.Store.HasUnsettledQuota(ctx, account.ID())
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
func (m *Monitor) SettleSoon(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer := m.settles[id]; timer != nil {
		timer.Reset(settleDelay)
		return
	}
	m.settles[id] = time.AfterFunc(settleDelay, func() {
		if account := m.cfg.Pool.Account(id); account != nil {
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
// when needed. An account whose sign-in agy refuses leaves rotation, as
// when a request meets that, until a new sign-in or successful refresh
// restores it.
func (m *Monitor) Refresh(ctx context.Context, account *pool.Account) (AccountQuota, error) {
	q, authOK, err := m.tryRefresh(ctx, account, cooldownRefresh)
	finishRefresh(account, authOK, err)
	return q, err
}

func finishRefresh(account *pool.Account, authOK bool, err error) {
	switch {
	case agy.IsAuthError(err):
		account.Disable(err)
	case err == nil || (authOK && account.Status().Verify):
		account.Enable()
	}
}

// TryRefresh is Refresh that leaves a refused sign-in to the caller, for
// trying a sign-in the account may not keep.
func (m *Monitor) TryRefresh(ctx context.Context, account *pool.Account) (AccountQuota, error) {
	q, _, err := m.tryRefresh(ctx, account, cooldownReplace)
	return q, err
}

func (m *Monitor) tryRefresh(ctx context.Context, account *pool.Account, mode cooldownSyncMode) (AccountQuota, bool, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	proc, err := account.Proc(ctx, m.cfg.Profile)
	probe := ""
	var models []agy.ModelInfo
	if err == nil {
		spec := agy.SessionSpec{Cwd: account.Workspace()}
		if account.Status().Disabled != "" || account.Models() == nil {
			probe, models, err = proc.ProbeLanguageServer(ctx, spec)
		} else {
			probe, models, err = proc.EnsureLanguageServer(ctx, spec)
		}
	}
	if probe != "" {
		account.SetModels(models)
		defer m.closeProbe(account.Name(), proc, probe)
	}
	if err != nil {
		m.fail(account, err)
		q, _ := m.Latest(account.ID())
		return q, false, err
	}
	q, err := m.sample(ctx, account, proc, mode)
	return q, true, err
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

func (m *Monitor) sample(ctx context.Context, account *pool.Account, proc *agy.Proc, mode cooldownSyncMode) (AccountQuota, error) {
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	id, name := account.ID(), account.Name()
	oldestRun, _ := account.OldestRun()
	user, err := proc.UserStatus(ctx)
	fetchedAt := time.Now()
	var groups []agy.QuotaGroup
	if err == nil {
		groups, err = proc.Quota(ctx)
	}
	if err != nil {
		m.fail(account, err)
		q, _ := m.Latest(id)
		return q, err
	}
	q := AccountQuota{At: time.Now(), User: user, Groups: groups}
	m.mu.Lock()
	prev := m.latest[id]
	m.latest[id] = q
	m.mu.Unlock()

	var samples []store.QuotaSample
	var bucketGroups []string
	for _, g := range groups {
		for _, b := range g.Buckets {
			bucketGroups = append(bucketGroups, BucketGroup(b.ID))
			samples = append(samples, store.QuotaSample{Account: id, At: q.At, Bucket: b.ID, Group: g.Name, Window: b.Window, Remaining: b.Remaining, ResetAt: b.ResetAt})
		}
	}
	account.SetQuotaGroups(bucketGroups)
	applySampleCooldowns(account, prev, q, fetchedAt, mode)
	if err := m.cfg.Store.AddQuotaSamples(ctx, samples); err != nil {
		m.cfg.Log.Error("quota samples not stored", "account", name, "err", err)
		return q, nil
	}
	m.expirePendingLocked(ctx)
	if m.cfg.OnSample != nil {
		m.cfg.OnSample(ctx, id, q.At, oldestRun)
	}
	return q, nil
}

// minQuotaResetJump is the minimum upward jump in a relevant bucket's
// remaining fraction that an explicit Refresh immediately trusts as a quota
// reset (well above Cloud Code's ~0.5% two-replica skew).
const minQuotaResetJump = 0.10

// resetMatchSkew tolerates minor clock/rounding differences between a 429
// RetryInfo deadline and RetrieveUserQuotaSummary's resetTime.
const resetMatchSkew = time.Minute

// quotaResetDebounce is how long after a cooldown was recorded an explicit
// Refresh waits before trusting remaining > 0 without an unambiguous refill or
// a jump >= minQuotaResetJump, covering Cloud Code's quota lag after a 429.
// A variable for tests.
var quotaResetDebounce = SettleTime

type cooldownSyncMode int

const (
	// cooldownBackground clears or shortens cooldowns only on an unambiguous refill.
	cooldownBackground cooldownSyncMode = iota
	// cooldownRefresh clears or shortens cooldowns on an unambiguous refill, a
	// remaining jump >= minQuotaResetJump, or once quotaResetDebounce has elapsed.
	cooldownRefresh
	// cooldownReplace unconditionally aligns cooldowns with the sample (e.g. a
	// newly imported sign-in replacing the previous Google account).
	cooldownReplace
)

// applySampleCooldowns updates account's quota cooldowns from a fresh sample.
func applySampleCooldowns(account *pool.Account, prev, cur AccountQuota, fetchedAt time.Time, mode cooldownSyncMode) {
	exhaustedUntil := map[string]time.Time{}
	seenGroups := map[string]bool{}
	for _, g := range cur.Groups {
		for _, b := range g.Buckets {
			group := BucketGroup(b.ID)
			if group == "" {
				continue
			}
			seenGroups[group] = true
			if b.Remaining <= 0 && b.ResetAt.After(cur.At) && b.ResetAt.After(exhaustedUntil[group]) {
				exhaustedUntil[group] = b.ResetAt
			}
		}
	}

	if mode == cooldownReplace {
		account.SyncCooldowns(exhaustedUntil, fetchedAt)
		return
	}

	cooldowns := account.Cooldowns()
	if mode == cooldownRefresh {
		// Clear cooldowns for groups absent from this account's quota plan
		// (e.g. a pre-sample argon cooldown on an Ultra account).
		for group := range cooldowns {
			if !seenGroups[group] {
				account.SetCooldown(group, time.Time{}, fetchedAt)
			}
		}
	}

	resetGroups := detectResetGroups(prev, cur, cooldowns, fetchedAt, mode)
	for group := range seenGroups {
		until := exhaustedUntil[group]
		if resetGroups[group] {
			account.SetCooldown(group, until, fetchedAt)
		} else if !until.IsZero() {
			account.Cool(group, until)
		}
	}
}

func detectResetGroups(prev, cur AccountQuota, cooldowns map[string]pool.Cooldown, fetchedAt time.Time, mode cooldownSyncMode) map[string]bool {
	if len(cooldowns) == 0 {
		return nil
	}

	prevBuckets := map[string]agy.QuotaBucket{}
	for _, g := range prev.Groups {
		for _, b := range g.Buckets {
			prevBuckets[b.ID] = b
		}
	}

	reset := map[string]bool{}
	if mode == cooldownRefresh {
		for group, cd := range cooldowns {
			if !fetchedAt.Before(cd.SetAt.Add(quotaResetDebounce)) {
				reset[group] = true
			}
		}
	}

	for _, g := range cur.Groups {
		for _, b := range g.Buckets {
			group := BucketGroup(b.ID)
			if group == "" || reset[group] {
				continue
			}
			cd, ok := cooldowns[group]
			if !ok {
				continue
			}
			pb, ok := prevBuckets[b.ID]
			if !ok {
				continue
			}

			// Require a later reset time (past both the previous sample and
			// the current cooldown) AND more remaining quota, matching
			// attribute.go's refill rule while ignoring 5h resets during a
			// weekly cooldown and rolling reset_at on untouched 100% buckets.
			unambiguousRefill := b.ResetAt.After(pb.ResetAt) && b.Remaining > pb.Remaining && b.ResetAt.After(cd.Until)
			if unambiguousRefill {
				reset[group] = true
				continue
			}

			if mode != cooldownRefresh {
				continue
			}

			// Ignore shorter-window buckets (e.g. 5h) whose reset times both
			// before and after the sample are far earlier than the cooldown.
			cooldownFloor := cd.Until.Add(-resetMatchSkew)
			relevantToCooldown := (!b.ResetAt.IsZero() && !b.ResetAt.Before(cooldownFloor)) ||
				(!pb.ResetAt.IsZero() && !pb.ResetAt.Before(cooldownFloor)) ||
				(b.ResetAt.IsZero() && pb.ResetAt.IsZero())
			significantJump := b.Remaining-pb.Remaining >= minQuotaResetJump
			if relevantToCooldown && significantJump {
				reset[group] = true
			}
		}
	}
	return reset
}

func (m *Monitor) fail(account *pool.Account, err error) {
	m.cfg.Log.Warn("quota sample failed", "account", account.Name(), "err", err)
	id := account.ID()
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.latest[id]
	q.Err, q.ErrAt = err.Error(), time.Now()
	m.latest[id] = q
}

// Latest is the account's newest sample, if any.
func (m *Monitor) Latest(id string) (AccountQuota, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.latest[id]
	return q, ok
}

// Forget drops a removed account's sample and pending settle.
func (m *Monitor) Forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.latest, id)
	if timer := m.settles[id]; timer != nil {
		timer.Stop()
		delete(m.settles, id)
	}
}
