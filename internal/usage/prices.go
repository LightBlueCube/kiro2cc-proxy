package usage

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Price is a model's API list price in USD per million tokens.
type Price struct {
	Model string `json:"model"`
	// Input is uncached prompt tokens; Cached is cache reads.
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	// CacheWrite, when above 0, prices all uncached prompt tokens instead
	// of Input, as if they were written to the cache; 0 means the model
	// has no cache-write price.
	CacheWrite float64 `json:"cache_write"`
	// Output includes reasoning tokens.
	Output float64 `json:"output"`
}

// DefaultPrices are the vendors' list prices as of 2026-11-13, from
// ai.google.dev/gemini-api/docs/pricing and
// platform.claude.com/docs/en/about-claude/pricing. gpt-oss-120b uses
// Google Vertex's price as OpenRouter lists it, which names no cached
// price, so cache reads cost as much as input. gemini-3.1-pro uses the
// "Gemini 3.1 Pro Preview" price for prompts up to 200k tokens; longer
// prompts cost more upstream ($4 / $0.40 / $18). Claude Opus 4.6 has one
// price, which covers agy's "-thinking" variant.
//
// agy reports no cache writes (cachedWriteTokens stays 0 on Claude while
// later turns read the cache). Measured on Claude Sonnet over three turns,
// a turn's uncached input matched what the next turn read from the cache
// within its newest message (29239 vs 29142 tokens), except the first
// turn, which cached about 12k of 41k. So Claude's uncached input is
// priced at Anthropic's 5-minute cache-write price (1.25 × input), which
// overstates first turns.
var DefaultPrices = []Price{
	{Model: "gemini-4-argon", Input: 2, Cached: 0.1, Output: 10},
	{Model: "gemini-3.8-flash", Input: 0.75, Cached: 0.075, Output: 3.75},
	{Model: "gemini-3.1-pro", Input: 2, Cached: 0.2, Output: 12},
	{Model: "claude-opus-5-5", Input: 4, Cached: 0.2, CacheWrite: 5, Output: 20},
	{Model: "claude-sonnet-5-5", Input: 2, Cached: 0.1, CacheWrite: 2.5, Output: 10},
	{Model: "claude-opus-4-6-thinking", Input: 5, Cached: 0.5, CacheWrite: 6.25, Output: 25},
	{Model: "claude-sonnet-4-6", Input: 3, Cached: 0.3, CacheWrite: 3.75, Output: 15},
	{Model: "gpt-oss-120b", Input: 0.09, Cached: 0.09, Output: 0.36},
}

// ValidatePrices reports the first malformed entry.
func ValidatePrices(prices []Price) error {
	seen := map[string]bool{}
	for _, p := range prices {
		if strings.TrimSpace(p.Model) == "" {
			return errors.New("every price needs a model id")
		}
		if p.Input < 0 || p.Cached < 0 || p.CacheWrite < 0 || p.Output < 0 {
			return fmt.Errorf("price for %s: prices cannot be negative", p.Model)
		}
		if seen[p.Model] {
			return fmt.Errorf("%s has two prices", p.Model)
		}
		seen[p.Model] = true
	}
	return nil
}

// priceOf finds the model's price.
func priceOf(prices []Price, model string) (Price, bool) {
	i := slices.IndexFunc(prices, func(p Price) bool { return p.Model == model })
	if i < 0 {
		return Price{}, false
	}
	return prices[i], true
}

// cost prices the tokens: prompt includes cached.
func (p Price) cost(prompt, cached, output int64) float64 {
	uncached := max(prompt-cached, 0)
	uncachedPrice := p.Input
	if p.CacheWrite > 0 {
		uncachedPrice = p.CacheWrite
	}
	return (float64(uncached)*uncachedPrice + float64(cached)*p.Cached + float64(output)*p.Output) / 1e6
}

func clonePrices(prices []Price) []Price { return slices.Clone(prices) }
