package gateway

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
)

// agy's catalog lists one slug per effort tier ("gemini-3.8-flash-low").
// The gateway serves one model per base slug and maps the request's
// reasoning effort onto the tiers that exist.
var (
	effortOrder = []string{"low", "medium", "high"}
	tierPattern = regexp.MustCompile(`^(.+)-(high|medium|low)$`)
	// Display names carry the tier: "Gemini 3.8 Flash (High)".
	tierNameSuffix = regexp.MustCompile(`\s+\((?i:high|medium|low)\)$`)
	effortCapable  = []*regexp.Regexp{regexp.MustCompile(`^gemini-`), regexp.MustCompile(`^claude-`), regexp.MustCompile(`^gpt-oss-`)}
)

type modelEntry struct {
	// id is the public model id: the base slug for effort-driven models,
	// else the exact ACP slug.
	id      string
	name    string
	efforts []string
}

// Offline catalog until an account reports its live one.
var fallbackModels = []modelEntry{
	{id: "gemini-4-argon", name: "Gemini 4 Argon", efforts: []string{"low", "medium", "high"}},
	{id: "gemini-3.8-flash", name: "Gemini 3.8 Flash", efforts: []string{"low", "medium", "high"}},
	{id: "gemini-3.1-pro", name: "Gemini 3.1 Pro", efforts: []string{"low", "high"}},
	{id: "claude-opus-5-5", name: "Claude Opus 5.5", efforts: []string{"low", "medium", "high"}},
	{id: "claude-sonnet-5-5", name: "Claude Sonnet 5.5", efforts: []string{"low", "medium", "high"}},
	{id: "gpt-oss-120b", name: "GPT-OSS 120B", efforts: []string{"medium"}},
}

type catalog struct {
	mu      sync.Mutex
	entries []modelEntry
	live    bool
}

func newCatalog() *catalog {
	return &catalog{entries: slices.Clone(fallbackModels)}
}

// update replaces the catalog with an account's live list.
func (c *catalog) update(models []agy.ModelInfo) {
	entries := entriesFromACP(models)
	if len(entries) == 0 {
		return
	}
	c.mu.Lock()
	c.entries = entries
	c.live = true
	c.mu.Unlock()
}

func (c *catalog) isLive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live
}

func (c *catalog) list() []conv.ModelInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]conv.ModelInfo, len(c.entries))
	for i, e := range c.entries {
		out[i] = conv.ModelInfo{ID: e.id, Name: e.name, Efforts: slices.Clone(e.efforts)}
	}
	return out
}

// resolve maps a public model id and requested effort to the full ACP slug.
// A tier slug ("gemini-3.8-flash-high") is accepted as is.
func (c *catalog) resolve(model, effort string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.id == model {
			if len(e.efforts) == 0 {
				return e.id, true
			}
			return e.id + "-" + pickEffort(effort, e.efforts), true
		}
	}
	if m := tierPattern.FindStringSubmatch(model); m != nil {
		for _, e := range c.entries {
			if e.id == m[1] && slices.Contains(e.efforts, m[2]) {
				return model, true
			}
		}
	}
	return "", false
}

// pickEffort maps a requested effort onto the available tiers: the wanted
// tier, else the next higher, else the next lower.
func pickEffort(requested string, efforts []string) string {
	wanted := "medium"
	switch requested {
	case "high", "xhigh", "max":
		wanted = "high"
	case "minimal", "low", "none":
		wanted = "low"
	case "medium":
	default:
		if !slices.Contains(efforts, "medium") {
			wanted = efforts[0]
		}
	}
	if slices.Contains(efforts, wanted) {
		return wanted
	}
	start := slices.Index(effortOrder, wanted)
	for i := start; i < len(effortOrder); i++ {
		if slices.Contains(efforts, effortOrder[i]) {
			return effortOrder[i]
		}
	}
	for i := start - 1; i >= 0; i-- {
		if slices.Contains(efforts, effortOrder[i]) {
			return effortOrder[i]
		}
	}
	return efforts[0]
}

// entriesFromACP groups tier variants of effort-capable families into one
// entry; unknown families stay exact so they never get an unverified tier.
func entriesFromACP(models []agy.ModelInfo) []modelEntry {
	type group struct {
		variants []agy.ModelInfo
		tiers    []string
	}
	groups := map[string]*group{}
	var order []string
	for _, model := range models {
		base, tier := model.ModelID, ""
		if m := tierPattern.FindStringSubmatch(model.ModelID); m != nil {
			base, tier = m[1], m[2]
		}
		g := groups[base]
		if g == nil {
			g = &group{}
			groups[base] = g
			order = append(order, base)
		}
		g.variants = append(g.variants, model)
		if tier != "" && !slices.Contains(g.tiers, tier) {
			g.tiers = append(g.tiers, tier)
		}
	}
	var entries []modelEntry
	for _, base := range order {
		g := groups[base]
		capable := slices.ContainsFunc(effortCapable, func(re *regexp.Regexp) bool { return re.MatchString(base) })
		if len(g.tiers) == 0 || !capable {
			for _, v := range g.variants {
				entries = append(entries, modelEntry{id: v.ModelID, name: v.Name})
			}
			continue
		}
		slices.SortFunc(g.tiers, func(a, b string) int { return slices.Index(effortOrder, a) - slices.Index(effortOrder, b) })
		name := base
		for _, v := range g.variants {
			if v.Name != "" {
				name = strings.TrimSpace(tierNameSuffix.ReplaceAllString(v.Name, ""))
				break
			}
		}
		entries = append(entries, modelEntry{id: base, name: name, efforts: g.tiers})
	}
	return entries
}
