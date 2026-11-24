package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

const (
	weeklyWindow = 7 * 24 * time.Hour
	// fullResetGrace covers Google's ~10m rolling reset_at updates on untouched
	// 100% weekly buckets plus clock skew. Once a weekly countdown starts, its
	// reset_at stays fixed and drops below sampledAt + 7d - fullResetGrace.
	fullResetGrace = 20 * time.Minute

	warmupRefreshTimeout = time.Minute
	warmupCallTimeout    = 2 * time.Minute
	warmupKeyName        = "warmup"
	maxWarmupModelLen    = 128
)

// Scheduling intervals (variables so tests can shorten them).
var (
	warmupAfterRefresh    = 30 * time.Second
	warmupResetMargin     = 2 * time.Minute
	warmupFreshWindow     = 2 * time.Minute
	warmupVerifySettle    = usage.SettleTime
	warmupVerifyDelay     = usage.SettleTime + 4*time.Minute
	warmupRetryBase       = 35 * time.Minute
	warmupFailRetryBase   = 10 * time.Minute
	warmupRetryMax        = 6 * time.Hour
	warmupRecentGuard     = 20 * time.Minute
	warmupBusyRetry       = time.Minute
	warmupRefreshBase     = 5 * time.Minute
	warmupRefreshMax      = time.Hour
	warmupFallbackRefresh = 6 * time.Hour
	warmupIdlePoll        = time.Hour
)

type warmupTracker struct {
	mu       sync.Mutex
	wake     chan struct{}
	accounts map[string]*accountWarmupState
}

type accountWarmupState struct {
	nextRefresh  time.Time
	refreshFails int
	verifyAt     time.Time
	verifySince  time.Time
	groups       map[string]*groupWarmupState
}

type groupWarmupState struct {
	lastAttempt time.Time
	nextAttempt time.Time
	streak      int
}

type weeklyGroupStatus struct {
	group     string
	bucket    agy.QuotaBucket
	exhausted bool
}

type warmupTarget struct {
	model  string
	slug   string
	effort string
}

func newWarmupTracker() *warmupTracker {
	return &warmupTracker{
		wake:     make(chan struct{}, 1),
		accounts: map[string]*accountWarmupState{},
	}
}

func (c *Control) wakeWarmup() {
	if c.warmup == nil {
		return
	}
	select {
	case c.warmup.wake <- struct{}{}:
	default:
	}
}

func (c *Control) resetAccountWarmup(id string) {
	if c.warmup == nil {
		return
	}
	c.warmup.mu.Lock()
	delete(c.warmup.accounts, id)
	c.warmup.mu.Unlock()
}

func (c *Control) onQuotaSample(accountID string, q usage.AccountQuota) {
	if c.warmup == nil {
		return
	}
	c.warmup.mu.Lock()
	st := c.warmup.accounts[accountID]
	if st != nil {
		st.refreshFails = 0
		st.nextRefresh = time.Time{}
		if !st.verifyAt.IsZero() && !q.At.Before(st.verifySince.Add(warmupVerifySettle)) {
			st.verifyAt = time.Time{}
			st.verifySince = time.Time{}
		}
	}
	c.warmup.mu.Unlock()
	if account := c.cfg.Pool.Account(accountID); c.Settings().WarmupFullPools && account != nil && account.InRotation() {
		c.wakeWarmup()
	}
}

func (w *warmupTracker) accountState(id string) *accountWarmupState {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.accounts[id]
	if st == nil {
		st = &accountWarmupState{groups: map[string]*groupWarmupState{}}
		w.accounts[id] = st
	}
	return st
}

// RunWarmup watches in-rotation accounts while WarmupFullPools is enabled and
// sends a minimal request to each full weekly quota pool whose 7-day reset
// countdown has not started yet.
func (c *Control) RunWarmup(ctx context.Context) {
	if c.warmup == nil || c.cfg.Gateway == nil || c.cfg.Monitor == nil {
		return
	}
	for {
		next := c.warmupPass(ctx, time.Now())
		if ctx.Err() != nil {
			return
		}
		if next.IsZero() {
			select {
			case <-ctx.Done():
				return
			case <-c.warmup.wake:
			}
			continue
		}
		wait := max(time.Until(next), 0)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.warmup.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// warmupPass inspects every in-rotation account once and returns the earliest
// time the next check should run (zero when WarmupFullPools is disabled).
func (c *Control) warmupPass(ctx context.Context, now time.Time) time.Time {
	settings := c.Settings()
	if !settings.WarmupFullPools {
		return time.Time{}
	}
	var nextWake time.Time
	schedule := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if nextWake.IsZero() || t.Before(nextWake) {
			nextWake = t
		}
	}
	for _, account := range c.cfg.Pool.Accounts() {
		if ctx.Err() != nil {
			return time.Time{}
		}
		if !account.InRotation() {
			continue
		}
		schedule(c.processAccountWarmup(ctx, account, settings, now))
		now = advanceNow(now)
	}
	if nextWake.IsZero() {
		return now.Add(warmupIdlePoll)
	}
	return nextWake
}

func (c *Control) processAccountWarmup(ctx context.Context, account *pool.Account, settings Settings, now time.Time) time.Time {
	sampledAt, groups, fromLive, hasAny := c.loadAccountQuota(ctx, account)
	if !hasAny {
		return time.Time{}
	}
	st := c.warmup.accountState(account.ID())

	var nextWake time.Time
	schedule := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if nextWake.IsZero() || t.Before(nextWake) {
			nextWake = t
		}
	}

	needRefresh, pendingAt := c.accountNeedsRefresh(ctx, account, st, sampledAt, groups, fromLive, now)
	schedule(pendingAt)

	if needRefresh {
		c.warmup.mu.Lock()
		nextRefresh := st.nextRefresh
		c.warmup.mu.Unlock()
		if now.Before(nextRefresh) {
			schedule(nextRefresh)
			return nextWake
		}
		refreshedAt, refreshedGroups, retryAt, ok := c.refreshForWarmup(ctx, account, st, now)
		if !ok {
			if !account.InRotation() {
				return time.Time{}
			}
			schedule(retryAt)
			return nextWake
		}
		sampledAt, groups = refreshedAt, refreshedGroups
		now = advanceNow(now)
	}

	if !account.InRotation() {
		return time.Time{}
	}

	warmedAny := false
	for _, g := range groups {
		if ctx.Err() != nil || !account.InRotation() {
			return time.Time{}
		}
		due, warmed, stop := c.warmAccountGroup(ctx, account, st, g, settings, sampledAt, now)
		schedule(due)
		if warmed {
			warmedAny = true
		}
		if stop {
			break
		}
		now = advanceNow(now)
	}

	if !account.InRotation() {
		return time.Time{}
	}

	if warmedAny {
		verifySince := advanceNow(now)
		verifyAt := verifySince.Add(warmupVerifyDelay)
		c.warmup.mu.Lock()
		st.verifySince = verifySince
		st.verifyAt = verifyAt
		c.warmup.mu.Unlock()
		schedule(verifyAt)
	} else {
		c.warmup.mu.Lock()
		verifyAt := st.verifyAt
		c.warmup.mu.Unlock()
		schedule(verifyAt)
	}

	return nextWake
}

func (c *Control) loadAccountQuota(ctx context.Context, account *pool.Account) (sampledAt time.Time, groups []weeklyGroupStatus, fromLive bool, hasAny bool) {
	if q, ok := c.cfg.Monitor.Latest(account.ID()); ok && !q.At.IsZero() {
		sampledAt, groups = weeklySnapshotFromLive(q)
		return sampledAt, groups, true, true
	}
	samples, err := c.cfg.Store.LatestQuotaSamples(ctx, account.ID())
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("reading stored quota samples for warmup failed", "account", account.Name(), "err", err)
		}
		return time.Time{}, nil, false, false
	}
	if len(samples) == 0 {
		return time.Time{}, nil, false, false
	}
	sampledAt, groups = weeklySnapshotFromStore(samples)
	return sampledAt, groups, false, true
}

func (c *Control) accountNeedsRefresh(ctx context.Context, account *pool.Account, st *accountWarmupState, sampledAt time.Time, groups []weeklyGroupStatus, fromLive bool, now time.Time) (bool, time.Time) {
	var pendingAt time.Time
	addPending := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if pendingAt.IsZero() || t.Before(pendingAt) {
			pendingAt = t
		}
	}
	c.warmup.mu.Lock()
	verifyAt := st.verifyAt
	verifySince := st.verifySince
	c.warmup.mu.Unlock()

	if !verifyAt.IsZero() {
		if fromLive && !sampledAt.Before(verifySince.Add(warmupVerifySettle)) {
			c.warmup.mu.Lock()
			st.verifyAt = time.Time{}
			st.verifySince = time.Time{}
			c.warmup.mu.Unlock()
		} else if !now.Before(verifyAt) {
			return true, time.Time{}
		} else {
			addPending(verifyAt)
		}
	}

	if now.Sub(sampledAt) >= warmupFallbackRefresh {
		return true, pendingAt
	}

	for _, g := range groups {
		if !g.bucket.ResetAt.IsZero() && !g.bucket.ResetAt.After(now) {
			resetReady := g.bucket.ResetAt.Add(warmupResetMargin)
			if now.Before(resetReady) {
				addPending(resetReady)
				continue
			}
			// If a live sample taken after reset_at still shows the old reset time,
			// wait warmupRefreshBase instead of tight-looping; after 1h fall back
			// to warmupFallbackRefresh.
			if fromLive && !sampledAt.Before(g.bucket.ResetAt) {
				if sampledAt.Sub(g.bucket.ResetAt) < time.Hour {
					retryAt := sampledAt.Add(warmupRefreshBase)
					if now.Before(retryAt) {
						addPending(retryAt)
						continue
					}
					return true, pendingAt
				}
			} else {
				return true, pendingAt
			}
		}
		cooling := g.exhausted || account.CooldownRemaining(g.group) > 0
		if cooling || !weeklyBucketNeedsWarmup(g.bucket, sampledAt) {
			continue
		}
		c.warmup.mu.Lock()
		var nextAttempt time.Time
		if gs := st.groups[g.group]; gs != nil {
			nextAttempt = gs.nextAttempt
		}
		c.warmup.mu.Unlock()
		if now.Before(nextAttempt) {
			continue
		}
		lastEnd, err := c.cfg.Store.LastAccountGroupRequestEnd(ctx, account.ID(), g.group, now.Add(-warmupRecentGuard))
		if err == nil && !lastEnd.IsZero() && now.Before(lastEnd.Add(warmupRecentGuard)) {
			continue
		}
		if !fromLive || account.Models() == nil || now.Sub(sampledAt) >= warmupFreshWindow {
			return true, pendingAt
		}
	}
	return false, pendingAt
}

func (c *Control) refreshForWarmup(ctx context.Context, account *pool.Account, st *accountWarmupState, now time.Time) (time.Time, []weeklyGroupStatus, time.Time, bool) {
	refreshCtx, cancel := context.WithTimeout(ctx, warmupRefreshTimeout)
	defer cancel()
	q, err := c.cfg.Monitor.Refresh(refreshCtx, account)
	if err != nil {
		if !account.InRotation() {
			return time.Time{}, nil, time.Time{}, false
		}
		c.warmup.mu.Lock()
		st.refreshFails++
		wait := warmupBackoff(warmupRefreshBase, warmupRefreshMax, st.refreshFails)
		st.nextRefresh = now.Add(wait)
		nextRetry := st.nextRefresh
		c.warmup.mu.Unlock()
		if ctx.Err() == nil {
			c.log.Warn("quota refresh for warmup failed", "account", account.Name(), "retry_in", wait, "err", err)
		}
		return time.Time{}, nil, nextRetry, false
	}
	c.warmup.mu.Lock()
	st.refreshFails = 0
	st.nextRefresh = time.Time{}
	if !st.verifyAt.IsZero() && !q.At.Before(st.verifySince.Add(warmupVerifySettle)) {
		st.verifyAt = time.Time{}
		st.verifySince = time.Time{}
	}
	c.warmup.mu.Unlock()
	sampledAt, groups := weeklySnapshotFromLive(q)
	return sampledAt, groups, time.Time{}, true
}

func (c *Control) warmAccountGroup(ctx context.Context, account *pool.Account, st *accountWarmupState, g weeklyGroupStatus, settings Settings, sampledAt, now time.Time) (nextAt time.Time, warmed bool, stop bool) {
	cooling := g.exhausted || account.CooldownRemaining(g.group) > 0
	if cooling || !weeklyBucketNeedsWarmup(g.bucket, sampledAt) {
		if !cooling {
			c.warmup.mu.Lock()
			delete(st.groups, g.group)
			c.warmup.mu.Unlock()
		}
		fallback := sampledAt.Add(warmupFallbackRefresh)
		if !g.bucket.ResetAt.IsZero() && g.bucket.ResetAt.After(now) {
			resetWake := g.bucket.ResetAt.Add(warmupResetMargin)
			if resetWake.Before(fallback) {
				return resetWake, false, false
			}
		}
		return fallback, false, false
	}

	c.warmup.mu.Lock()
	gs := st.groups[g.group]
	if gs == nil {
		gs = &groupWarmupState{}
		st.groups[g.group] = gs
	}
	nextAttempt := gs.nextAttempt
	c.warmup.mu.Unlock()

	if now.Before(nextAttempt) {
		return nextAttempt, false, false
	}

	lastEnd, err := c.cfg.Store.LastAccountGroupRequestEnd(ctx, account.ID(), g.group, now.Add(-warmupRecentGuard))
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("checking recent usage before warmup failed", "account", account.Name(), "group", g.group, "err", err)
		}
		return now.Add(warmupBusyRetry), false, false
	}
	if !lastEnd.IsZero() && now.Before(lastEnd.Add(warmupRecentGuard)) {
		return lastEnd.Add(warmupRecentGuard), false, false
	}
	if readyAt := sampledAt.Add(warmupAfterRefresh); now.Before(readyAt) {
		return readyAt, false, false
	}

	configured := settings.WarmupModels[g.group]
	target, configuredMissing, ok := pickWarmupModel(c.cfg.Gateway.AccountModels(account.ID()), g.group, configured, settings.Prices)
	if !ok {
		c.warmup.mu.Lock()
		gs.nextAttempt = now.Add(warmupRetryMax)
		c.warmup.mu.Unlock()
		c.log.Warn("no offered model available for weekly quota warmup", "account", account.Name(), "group", g.group)
		return now.Add(warmupRetryMax), false, false
	}
	if configuredMissing {
		c.log.Warn("configured warmup model not offered by account; falling back to cheapest", "account", account.Name(), "group", g.group, "configured", configured, "fallback", target.slug)
	}

	callCtx, cancel := context.WithTimeout(ctx, warmupCallTimeout)
	err = c.cfg.Gateway.Warmup(callCtx, account, gateway.SystemCall{
		KeyHash: store.SystemKeyWarmup,
		KeyName: warmupKeyName,
		Model:   target.model,
		Slug:    target.slug,
		Effort:  target.effort,
		Group:   g.group,
	})
	cancel()

	if errors.Is(err, gateway.ErrSlotsBusy) {
		return now.Add(warmupBusyRetry), false, false
	}
	if err != nil {
		if !account.InRotation() {
			return time.Time{}, false, true
		}
		c.warmup.mu.Lock()
		gs.streak++
		gs.lastAttempt = now
		wait := warmupBackoff(warmupFailRetryBase, warmupRetryMax, gs.streak)
		gs.nextAttempt = now.Add(wait)
		nextRetry := gs.nextAttempt
		c.warmup.mu.Unlock()
		if ctx.Err() == nil {
			c.log.Warn("weekly quota warmup failed", "account", account.Name(), "group", g.group, "model", target.model, "slug", target.slug, "retry_in", wait, "err", err)
		}
		return nextRetry, false, false
	}

	c.warmup.mu.Lock()
	gs.streak++
	streak := gs.streak
	gs.lastAttempt = now
	wait := warmupBackoff(warmupRetryBase, warmupRetryMax, streak)
	gs.nextAttempt = now.Add(wait)
	nextRetry := gs.nextAttempt
	c.warmup.mu.Unlock()
	if streak > 1 {
		c.log.Warn("weekly quota bucket remained full after earlier warmup; retried", "account", account.Name(), "group", g.group, "model", target.model, "slug", target.slug, "attempt", streak, "next_retry_in", wait)
	} else {
		c.log.Info("weekly quota warmup sent", "account", account.Name(), "group", g.group, "model", target.model, "slug", target.slug)
	}
	return nextRetry, true, !account.InRotation()
}

func advanceNow(now time.Time) time.Time {
	if cur := time.Now(); cur.After(now) {
		return cur
	}
	return now
}

func weeklySnapshotFromLive(q usage.AccountQuota) (time.Time, []weeklyGroupStatus) {
	byGroup := map[string]*weeklyGroupStatus{}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			recordBucketStatus(byGroup, b)
		}
	}
	return q.At, orderedWeeklyGroups(byGroup)
}

func weeklySnapshotFromStore(samples []store.QuotaSample) (time.Time, []weeklyGroupStatus) {
	byGroup := map[string]*weeklyGroupStatus{}
	var latest time.Time
	for _, s := range samples {
		if s.At.After(latest) {
			latest = s.At
		}
		recordBucketStatus(byGroup, agy.QuotaBucket{
			ID:        s.Bucket,
			Window:    s.Window,
			Remaining: s.Remaining,
			ResetAt:   s.ResetAt,
		})
	}
	return latest, orderedWeeklyGroups(byGroup)
}

func recordBucketStatus(byGroup map[string]*weeklyGroupStatus, b agy.QuotaBucket) {
	group := usage.BucketGroup(b.ID)
	if group == "" {
		return
	}
	entry := byGroup[group]
	if entry == nil {
		entry = &weeklyGroupStatus{group: group}
		byGroup[group] = entry
	}
	if b.Remaining <= 0 {
		entry.exhausted = true
	}
	if isWeeklyBucket(b) {
		entry.bucket = b
	}
}

func isWeeklyBucket(b agy.QuotaBucket) bool {
	if b.Window != "" {
		return b.Window == "weekly"
	}
	return strings.HasSuffix(b.ID, "-weekly")
}

func orderedWeeklyGroups(byGroup map[string]*weeklyGroupStatus) []weeklyGroupStatus {
	var out []weeklyGroupStatus
	seen := map[string]bool{}
	for _, group := range usage.Groups {
		entry := byGroup[group]
		if entry != nil && entry.bucket.ID != "" {
			out = append(out, *entry)
			seen[group] = true
		}
	}
	var extra []string
	for group, entry := range byGroup {
		if !seen[group] && entry.bucket.ID != "" {
			extra = append(extra, group)
		}
	}
	slices.Sort(extra)
	for _, group := range extra {
		out = append(out, *byGroup[group])
	}
	return out
}

// weeklyBucketNeedsWarmup reports whether a weekly bucket is at 100% and its
// 7-day reset countdown has not started yet.
func weeklyBucketNeedsWarmup(b agy.QuotaBucket, sampledAt time.Time) bool {
	if b.Remaining < 1.0 {
		return false
	}
	if b.ResetAt.IsZero() || !b.ResetAt.After(sampledAt) {
		return true
	}
	return b.ResetAt.Sub(sampledAt) >= weeklyWindow-fullResetGrace
}

// pickWarmupModel picks the model and ACP slug to warm up group, ignoring
// per-account allowed_models routing restrictions. When configured is non-empty
// and offered in group, it is used; otherwise the cheapest offered model in
// group according to prices (using its lowest effort tier) is selected.
func pickWarmupModel(catalog []conv.ModelInfo, group, configured string, prices []usage.Price) (target warmupTarget, configuredMissing bool, ok bool) {
	var candidates []conv.ModelInfo
	for _, m := range catalog {
		if pool.QuotaGroup(m.ID) == group {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return warmupTarget{}, false, false
	}

	configured = strings.TrimSpace(configured)
	if configured != "" {
		if t, found := matchConfiguredModel(candidates, configured); found {
			return t, false, true
		}
		configuredMissing = true
	}

	return lowestEffortTarget(cheapestModel(candidates, prices)), configuredMissing, true
}

// cheapestModel returns the model of candidates (not empty) with the lowest
// unit cost; models without a price come last.
func cheapestModel(candidates []conv.ModelInfo, prices []usage.Price) conv.ModelInfo {
	sorted := slices.Clone(candidates)
	slices.SortFunc(sorted, func(a, b conv.ModelInfo) int {
		costA, pricedA := usage.UnitCost(prices, a.ID)
		costB, pricedB := usage.UnitCost(prices, b.ID)
		if pricedA != pricedB {
			if pricedA {
				return -1
			}
			return 1
		}
		if pricedA {
			if c := cmp.Compare(costA, costB); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return sorted[0]
}

func matchConfiguredModel(candidates []conv.ModelInfo, configured string) (warmupTarget, bool) {
	for _, m := range candidates {
		if m.ID == configured {
			return lowestEffortTarget(m), true
		}
		if i := slices.Index(m.Slugs, configured); i >= 0 {
			effort := ""
			if i < len(m.Efforts) {
				effort = m.Efforts[i]
			}
			return warmupTarget{model: m.ID, slug: m.Slugs[i], effort: effort}, true
		}
	}
	return warmupTarget{}, false
}

func lowestEffortTarget(m conv.ModelInfo) warmupTarget {
	if len(m.Efforts) > 0 && len(m.Slugs) > 0 {
		return warmupTarget{model: m.ID, slug: m.Slugs[0], effort: m.Efforts[0]}
	}
	if len(m.Slugs) > 0 {
		return warmupTarget{model: m.ID, slug: m.Slugs[0]}
	}
	return warmupTarget{model: m.ID, slug: m.ID}
}

func warmupBackoff(base, limit time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return min(base, limit)
	}
	d := base
	for i := 1; i < attempt && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func cleanWarmupModels(raw map[string]string) map[string]string {
	out := map[string]string{}
	for group, model := range raw {
		g := strings.TrimSpace(group)
		m := strings.TrimSpace(model)
		if m == "" {
			continue
		}
		out[g] = m
	}
	return out
}

func validateWarmupModels(models map[string]string) error {
	for group, model := range models {
		if !slices.Contains(usage.Groups, group) {
			return fmt.Errorf("warmup_models: unknown quota group %q", group)
		}
		if model == "" {
			continue
		}
		if len(model) > maxWarmupModelLen || strings.ContainsFunc(model, unicode.IsControl) || strings.ContainsAny(model, " \t\r\n") {
			return fmt.Errorf("warmup_models[%s]: invalid model %q", group, model)
		}
		if !warmupModelAllowedForGroup(group, model) {
			return fmt.Errorf("warmup_models[%s]: model %q belongs to a different quota group", group, model)
		}
	}
	return nil
}

func warmupModelAllowedForGroup(group, model string) bool {
	switch {
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss-"):
		return group == pool.GroupClaudeGPT
	case strings.HasPrefix(model, "gemini-"):
		return group == pool.GroupGemini
	default:
		return true
	}
}
