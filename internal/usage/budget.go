package usage

import (
	"context"
	"time"

	"agyrelay/internal/store"
)

// BudgetWindow is the span API key limits count: the last 7 days.
const BudgetWindow = 7 * 24 * time.Hour

// Budget measures API keys' weekly-quota use against their limits.
type Budget struct {
	store *store.Store
	now   func() time.Time
}

func NewBudget(st *store.Store) *Budget { return &Budget{store: st, now: time.Now} }

// Use is a key's weekly-quota use in one quota group, as a share of one
// account's weekly bucket.
type Use struct {
	// Attributed is the sum of the shares attributed from quota samples.
	Attributed float64
	// Estimated covers requests not attributed yet: their list-price cost
	// times the quota share per dollar their account's attributed requests
	// used (or, without those, the quota group's across accounts).
	// Unpriced requests are estimated as 0.
	Estimated float64
}

func (u Use) Total() float64 { return u.Attributed + u.Estimated }

// Uses returns quota use over BudgetWindow by key and quota group; key ""
// selects every key.
func (b *Budget) Uses(ctx context.Context, key string) (map[string]map[string]Use, error) {
	since := b.now().Add(-BudgetWindow)
	rows, err := b.store.QuotaUses(ctx, key, since)
	if err != nil {
		return nil, err
	}
	rates, err := b.store.QuotaRates(ctx, since)
	if err != nil {
		return nil, err
	}
	rate := rateLookup(rates)

	out := map[string]map[string]Use{}
	for _, row := range rows {
		byGroup := out[row.Key]
		if byGroup == nil {
			byGroup = map[string]Use{}
			out[row.Key] = byGroup
		}
		use := byGroup[row.Group]
		use.Attributed += row.Attributed
		use.Estimated += row.PendingCostUSD * rate(row.Group, row.Account)
		byGroup[row.Group] = use
	}
	return out, nil
}

// Used implements gateway.Budget.
func (b *Budget) Used(ctx context.Context, key, group string) (float64, error) {
	uses, err := b.Uses(ctx, key)
	if err != nil {
		return 0, err
	}
	return uses[key][group].Total(), nil
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
