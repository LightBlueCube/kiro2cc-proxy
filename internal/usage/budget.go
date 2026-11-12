package usage

import (
	"context"
	"time"

	"agyrelay/internal/store"
)

// rateWindow is how far back attributed requests set the quota share per
// dollar that estimates requests not attributed yet.
const rateWindow = 7 * 24 * time.Hour

// Budget measures API keys' quota use against their limits. Use counts
// since the key was created and never resets.
type Budget struct {
	store *store.Store
	now   func() time.Time
}

func NewBudget(st *store.Store) *Budget { return &Budget{store: st, now: time.Now} }

// Use is a key's quota use in one quota group, as a share of one
// account's weekly bucket.
type Use struct {
	// Attributed is the sum of the shares attributed from quota samples.
	Attributed float64
	// Estimated covers requests not attributed yet: their list-price cost
	// times the quota share per dollar their account's attributed requests
	// used over rateWindow (or, without those, the quota group's across
	// accounts). Unpriced requests are estimated as 0.
	Estimated float64
}

func (u Use) Total() float64 { return u.Attributed + u.Estimated }

// Uses returns quota use by key hash and quota group; keyHash "" selects
// every key.
func (b *Budget) Uses(ctx context.Context, keyHash string) (map[string]map[string]Use, error) {
	attributed, err := b.store.KeyUses(ctx, keyHash)
	if err != nil {
		return nil, err
	}
	pending, err := b.store.PendingCosts(ctx, keyHash)
	if err != nil {
		return nil, err
	}
	rates, err := b.store.QuotaRates(ctx, b.now().Add(-rateWindow))
	if err != nil {
		return nil, err
	}
	rate := rateLookup(rates)

	out := map[string]map[string]Use{}
	add := func(keyHash, group string, attributed, estimated float64) {
		byGroup := out[keyHash]
		if byGroup == nil {
			byGroup = map[string]Use{}
			out[keyHash] = byGroup
		}
		use := byGroup[group]
		use.Attributed += attributed
		use.Estimated += estimated
		byGroup[group] = use
	}
	for _, u := range attributed {
		add(u.KeyHash, u.Group, u.Used, 0)
	}
	for _, p := range pending {
		add(p.KeyHash, p.Group, 0, p.CostUSD*rate(p.Group, p.Account))
	}
	return out, nil
}

// Used implements gateway.Budget.
func (b *Budget) Used(ctx context.Context, keyHash, group string) (float64, error) {
	uses, err := b.Uses(ctx, keyHash)
	if err != nil {
		return 0, err
	}
	return uses[keyHash][group].Total(), nil
}

// rateLookup returns quota share per dollar by (group, account), falling
// back to the group's rate over all accounts, then to 0.
func rateLookup(rates []store.QuotaRate) func(group, account string) float64 {
	type total struct{ attributed, cost float64 }
	byAccount := map[[2]string]total{}
	byGroup := map[string]total{}
	for _, r := range rates {
		byAccount[[2]string{r.Group, r.Account}] = total{r.Attributed, r.CostUSD}
		g := byGroup[r.Group]
		g.attributed += r.Attributed
		g.cost += r.CostUSD
		byGroup[r.Group] = g
	}
	return func(group, account string) float64 {
		if t, ok := byAccount[[2]string{group, account}]; ok {
			return t.attributed / t.cost
		}
		if t, ok := byGroup[group]; ok {
			return t.attributed / t.cost
		}
		return 0
	}
}
