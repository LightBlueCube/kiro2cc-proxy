package usage

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Price is a model's API list price in USD per million tokens, effective
// from From (a UTC date, "" = always) until a later entry of the model.
type Price struct {
	Model string `json:"model"`
	From  string `json:"from"`
	// Input is uncached prompt tokens; Cached is cache reads.
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	// Output includes reasoning tokens.
	Output float64 `json:"output"`
}

const dateLayout = "2006-01-02"

// DefaultPrices are the vendors' list prices as of 2026-11-10, from
// ai.google.dev/gemini-api/docs/pricing and
// platform.claude.com/docs/en/about-claude/pricing. Google lists the
// later increases already. gpt-oss-120b has no official price.
var DefaultPrices = []Price{
	{Model: "gemini-4-argon", Input: 2, Cached: 0.1, Output: 10},
	{Model: "gemini-4-argon", From: "2026-12-01", Input: 4, Cached: 0.2, Output: 20},
	{Model: "gemini-3.8-flash", Input: 0.75, Cached: 0.075, Output: 3.75},
	{Model: "gemini-3.8-flash", From: "2027-01-01", Input: 1.5, Cached: 0.15, Output: 7.5},
	{Model: "claude-opus-5-5", Input: 4, Cached: 0.2, Output: 20},
	{Model: "claude-sonnet-5-5", Input: 2, Cached: 0.1, Output: 10},
}

// ValidatePrices reports the first malformed entry.
func ValidatePrices(prices []Price) error {
	seen := map[string]bool{}
	for _, p := range prices {
		if strings.TrimSpace(p.Model) == "" {
			return errors.New("every price needs a model id")
		}
		if p.From != "" {
			if _, err := time.Parse(dateLayout, p.From); err != nil {
				return fmt.Errorf("price for %s: from must be YYYY-MM-DD", p.Model)
			}
		}
		if p.Input < 0 || p.Cached < 0 || p.Output < 0 {
			return fmt.Errorf("price for %s: prices cannot be negative", p.Model)
		}
		key := p.Model + "\x00" + p.From
		if seen[key] {
			return fmt.Errorf("price for %s from %q is listed twice", p.Model, p.From)
		}
		seen[key] = true
	}
	return nil
}

// priceAt finds the model's price in effect at t.
func priceAt(prices []Price, model string, t time.Time) (Price, bool) {
	day := t.UTC().Format(dateLayout)
	var best Price
	found := false
	for _, p := range prices {
		if p.Model != model || p.From > day {
			continue
		}
		if !found || p.From > best.From {
			best, found = p, true
		}
	}
	return best, found
}

// cost prices the tokens: prompt includes cached.
func (p Price) cost(prompt, cached, output int64) float64 {
	uncached := max(prompt-cached, 0)
	return (float64(uncached)*p.Input + float64(cached)*p.Cached + float64(output)*p.Output) / 1e6
}

func clonePrices(prices []Price) []Price { return slices.Clone(prices) }
