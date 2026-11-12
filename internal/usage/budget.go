package usage

import (
	"context"

	"agyrelay/internal/store"
)

// Budget measures API keys' list-price spend against their limits. Spend
// counts since the key was created and never resets.
type Budget struct {
	store  *store.Store
	prices *Recorder
}

// NewBudget prices models with the recorder's price table.
func NewBudget(st *store.Store, prices *Recorder) *Budget {
	return &Budget{store: st, prices: prices}
}

// Spends returns spend in USD by key hash and quota group; keyHash ""
// selects every key.
func (b *Budget) Spends(ctx context.Context, keyHash string) (map[string]map[string]float64, error) {
	spends, err := b.store.KeySpends(ctx, keyHash)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, s := range spends {
		if out[s.KeyHash] == nil {
			out[s.KeyHash] = map[string]float64{}
		}
		out[s.KeyHash][s.Group] = s.CostUSD
	}
	return out, nil
}

// Spent implements gateway.Budget.
func (b *Budget) Spent(ctx context.Context, keyHash string) (map[string]float64, error) {
	spends, err := b.Spends(ctx, keyHash)
	if err != nil {
		return nil, err
	}
	return spends[keyHash], nil
}

// Priced implements gateway.Budget.
func (b *Budget) Priced(model string) bool { return b.prices.priced(model) }
