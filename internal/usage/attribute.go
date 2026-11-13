package usage

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"agyrelay/internal/store"
)

// weeklyBuckets maps agy's weekly quota buckets to the pool's quota groups
// (pool.QuotaGroup). Bucket ids as agy 1.5.3 reports them for a Pro plan.
var weeklyBuckets = map[string]string{
	"gemini-weekly":           "gemini",
	"3p-weekly":               "claude-gpt",
	"gemini-argon-wtu-weekly": "argon",
}

// Groups lists the quota groups with a weekly bucket.
var Groups = []string{"gemini", "claude-gpt", "argon"}

// BucketGroup maps a quota bucket id to its pool quota group.
func BucketGroup(bucketID string) string {
	if g, ok := weeklyBuckets[bucketID]; ok {
		return g
	}
	switch {
	case strings.HasPrefix(bucketID, "3p-"):
		return "claude-gpt"
	case strings.HasPrefix(bucketID, "gemini-argon-"):
		return "argon"
	case strings.HasPrefix(bucketID, "gemini-"):
		return "gemini"
	default:
		return ""
	}
}

// QuotaReport is the weekly-quota use attributed to matching requests.
// Shares of different accounts' buckets do not add up to one number: plans
// differ in bucket size.
type QuotaReport struct {
	// Accounts lists by quota group the accounts that have the group's
	// weekly bucket: configured accounts sampled with it, and accounts
	// with use in Uses. A key's average over accounts divides by them.
	Accounts map[string][]string `json:"accounts"`
	Uses     []store.QuotaUse    `json:"uses"`
}

func Quota(ctx context.Context, st *store.Store, f store.RequestFilter) (QuotaReport, error) {
	uses, err := st.QuotaUses(ctx, f)
	if err != nil {
		return QuotaReport{}, err
	}
	byBucket, err := st.BucketAccounts(ctx)
	if err != nil {
		return QuotaReport{}, err
	}
	report := QuotaReport{Accounts: map[string][]string{}, Uses: uses}
	add := func(group, account string) {
		if !slices.Contains(report.Accounts[group], account) {
			report.Accounts[group] = append(report.Accounts[group], account)
		}
	}
	for bucket, group := range weeklyBuckets {
		for _, account := range byBucket[bucket] {
			add(group, account)
		}
	}
	for _, u := range uses {
		add(u.Group, u.Account)
	}
	for _, accounts := range report.Accounts {
		slices.Sort(accounts)
	}
	if report.Uses == nil {
		report.Uses = []store.QuotaUse{}
	}
	return report, nil
}

// SettleTime is how long after a request ends its use is taken to show in
// the account's quota. Measured on gemini-weekly (2026-11-14): sampled
// every few seconds, the fraction moved 260-311 s after each of 7
// requests ended; sampled rarely, twice only after 8 and 12 minutes. A
// drop that has not shown yet waits (drop <= 0); see Monitor.KeepForQuota.
const SettleTime = 6 * time.Minute

// Attributor splits weekly-bucket drops over the account's requests of
// that quota group. Google charges quota in proportion to token cost, so
// requests weigh by list-price cost, or by tokens when any of them is
// unpriced.
//
// Each bucket has a cursor: the sample up to which drops are settled. A
// sample settles the drop since the cursor over the unsettled requests
// that ended by the sample, only when no turn is running (a running
// turn's record, with its tokens, comes when it ends) and the newest of
// them ended SettleTime before; otherwise the drop grows into the next
// sample.
//
//	cursor ──── requests ──── sample (busy: carry) ──── sample (idle: split drop)
//
// Requests are picked by end time, not by the cursor, so one stored after
// a sample passed its start is settled by a later sample. A drop with no
// such request is use outside agyrelay and stays unattributed. Requests
// that cannot be settled get a skip reason (store.QuotaSkip).
type Attributor struct {
	store *store.Store
	log   *slog.Logger
}

func NewAttributor(st *store.Store, log *slog.Logger) *Attributor {
	return &Attributor{store: st, log: log}
}

// Attribute runs after the account was sampled at `at`; busy reports
// whether a turn was running or parked on the account then.
func (a *Attributor) Attribute(ctx context.Context, account string, at time.Time, busy bool) {
	for bucket, group := range weeklyBuckets {
		if err := a.attributeBucket(ctx, account, bucket, group, at, busy); err != nil {
			a.log.Warn("quota attribution failed", "account", account, "bucket", bucket, "err", err)
		}
	}
}

func (a *Attributor) attributeBucket(ctx context.Context, account, bucket, group string, at time.Time, busy bool) error {
	cur, err := a.store.LastQuotaSample(ctx, account, bucket, at.Add(time.Millisecond))
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !found || cur.At.UnixMilli() != at.UnixMilli() {
		return a.store.SkipQuota(ctx, account, group, at, store.SkipNoBucket)
	}
	from, err := a.store.QuotaCursor(ctx, account, bucket)
	if errors.Is(err, store.ErrNotFound) {
		return a.restart(ctx, account, bucket, group, cur.At, store.SkipBeforeSampling)
	}
	if err != nil {
		return err
	}
	// A rise without a later reset time is a stale sample, not a reset:
	// Cloud Code answers some reads with an older value (seen: 0.9897,
	// 0.9935, 0.9897 under one reset time). It falls through to drop <= 0
	// and leaves the cursor, so the drop it hid is counted once.
	refilled := cur.ResetAt.After(from.ResetAt) && cur.Remaining > from.Remaining
	if !cur.At.Before(from.ResetAt) || refilled {
		return a.restart(ctx, account, bucket, group, cur.At, store.SkipReset)
	}
	if busy {
		return nil
	}

	weights, err := a.store.QuotaWeights(ctx, account, group, cur.At)
	if err != nil {
		return err
	}
	if len(weights) == 0 {
		return a.store.AttributeQuota(ctx, account, bucket, cur.At, nil)
	}
	drop := from.Remaining - cur.Remaining
	if drop <= 0 || cur.At.Sub(newestEnd(weights)) < SettleTime {
		return nil
	}
	return a.store.AttributeQuota(ctx, account, bucket, cur.At, split(drop, weights))
}

// restart begins the bucket's attribution at the sample at `at`: the
// unsettled requests before it have no base to settle against.
func (a *Attributor) restart(ctx context.Context, account, bucket, group string, at time.Time, reason store.QuotaSkip) error {
	if err := a.store.SkipQuota(ctx, account, group, at, reason); err != nil {
		return err
	}
	return a.store.AttributeQuota(ctx, account, bucket, at, nil)
}

func newestEnd(weights []store.QuotaWeight) time.Time {
	var newest time.Time
	for _, w := range weights {
		if w.Ended.After(newest) {
			newest = w.Ended
		}
	}
	return newest
}

// split shares drop by cost, or by tokens when a request has no price.
func split(drop float64, weights []store.QuotaWeight) map[int64]float64 {
	byCost := true
	for _, w := range weights {
		if w.CostUSD == nil {
			byCost = false
		}
	}
	weightOf := func(w store.QuotaWeight) float64 {
		if byCost {
			return *w.CostUSD
		}
		return float64(w.Tokens)
	}
	total := 0.0
	for _, w := range weights {
		total += weightOf(w)
	}
	shares := make(map[int64]float64, len(weights))
	for _, w := range weights {
		if total == 0 {
			shares[w.ID] = drop / float64(len(weights))
			continue
		}
		shares[w.ID] = drop * weightOf(w) / total
	}
	return shares
}
