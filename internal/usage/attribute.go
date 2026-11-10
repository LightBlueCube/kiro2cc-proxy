package usage

import (
	"context"
	"errors"
	"log/slog"
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

// Attributor splits each drop of a weekly bucket between two samples over
// the account's requests of that quota group in the interval. Google
// charges quota in proportion to token cost, so requests weigh by
// list-price cost, or by tokens when any of them is unpriced. A drop with
// no request in its interval (use outside agyrelay, or the sample lag
// pushing usage past a boundary) stays unattributed.
type Attributor struct {
	store *store.Store
	log   *slog.Logger
}

func NewAttributor(st *store.Store, log *slog.Logger) *Attributor {
	return &Attributor{store: st, log: log}
}

// Attribute runs after the account was sampled at `at`.
func (a *Attributor) Attribute(ctx context.Context, account string, at time.Time) {
	for bucket, group := range weeklyBuckets {
		if err := a.attributeBucket(ctx, account, bucket, group, at); err != nil {
			a.log.Warn("quota attribution failed", "account", account, "bucket", bucket, "err", err)
		}
	}
}

func (a *Attributor) attributeBucket(ctx context.Context, account, bucket, group string, at time.Time) error {
	cur, err := a.store.LastQuotaSample(ctx, account, bucket, at.Add(time.Millisecond))
	if errors.Is(err, store.ErrNotFound) {
		return nil // this plan has no such bucket
	}
	if err != nil {
		return err
	}
	prev, err := a.store.LastQuotaSample(ctx, account, bucket, cur.At)
	if errors.Is(err, store.ErrNotFound) {
		return nil // first sample
	}
	if err != nil {
		return err
	}
	// A rise means the bucket reset; what was used before is unknown.
	drop := prev.Remaining - cur.Remaining
	if drop <= 0 {
		return nil
	}
	weights, err := a.store.QuotaWeights(ctx, account, group, prev.At, cur.At)
	if err != nil || len(weights) == 0 {
		return err
	}
	return a.store.SetQuotaUsed(ctx, split(drop, weights))
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
