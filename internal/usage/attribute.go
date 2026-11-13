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

// settleTime is how long after a request its quota is assumed to show in
// samples. Measured once: the fraction had not moved 20 s after a turn.
const settleTime = 2 * time.Minute

// Attributor splits weekly-bucket drops over the account's requests of
// that quota group. Google charges quota in proportion to token cost, so
// requests weigh by list-price cost, or by tokens when any of them is
// unpriced.
//
// Each bucket has a cursor: the sample up to which drops are settled. A
// sample closes the interval since the cursor only when no turn is running
// (a running turn's record, with its tokens, comes when it ends) and the
// newest request had settleTime to show in the quota; otherwise the
// interval grows into the next sample.
//
//	cursor ──── requests ──── sample (busy: carry) ──── sample (idle: split drop)
//
// A drop with no request in a closed interval is use outside agyrelay and
// stays unattributed, as does use before a bucket reset.
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
	if errors.Is(err, store.ErrNotFound) {
		return nil // this plan has no such bucket
	}
	if err != nil {
		return err
	}
	from, err := a.store.QuotaCursor(ctx, account, bucket)
	if errors.Is(err, store.ErrNotFound) {
		return a.store.AttributeQuota(ctx, account, bucket, cur.At, nil) // first sample
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
		return a.store.AttributeQuota(ctx, account, bucket, cur.At, nil)
	}
	if busy {
		return nil
	}

	weights, err := a.store.QuotaWeights(ctx, account, group, from.At, cur.At)
	if err != nil {
		return err
	}
	if len(weights) == 0 {
		return a.store.AttributeQuota(ctx, account, bucket, cur.At, nil)
	}
	drop := from.Remaining - cur.Remaining
	if drop <= 0 || cur.At.Sub(newestEnd(weights)) < settleTime {
		return nil
	}
	return a.store.AttributeQuota(ctx, account, bucket, cur.At, split(drop, weights))
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
