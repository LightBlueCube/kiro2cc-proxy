package gateway

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
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

// Models lists what the accounts offer.
func (g *Gateway) Models() []conv.ModelInfo { return modelInfos(g.offered()) }

// offered merges the catalogs agy reported for the accounts, a model's
// tiers being those of any account; before any report it is the fallback
// list.
func (g *Gateway) offered() []modelEntry {
	var merged []modelEntry
	reported := false
	for _, account := range g.pool.Accounts() {
		models := account.Models()
		if models == nil {
			continue
		}
		reported = true
		for _, e := range entriesFromACP(models) {
			i := slices.IndexFunc(merged, func(m modelEntry) bool { return m.id == e.id })
			if i < 0 {
				merged = append(merged, e)
				continue
			}
			merged[i].efforts = mergeEfforts(merged[i].efforts, e.efforts)
		}
	}
	if !reported {
		return fallbackModels
	}
	return merged
}

const (
	// refreshTimeout bounds one refresh (about 3.5 s offline with agy not
	// yet running).
	refreshTimeout = time.Minute
	// refreshRetry is how long an account a refresh left without a
	// catalog is not refreshed again for requests.
	refreshRetry = time.Minute
)

type refreshCall struct {
	done     chan struct{}
	failedAt time.Time // zero while running
}

// refreshUnreported refreshes every account in rotation that has not
// reported a catalog since the relay started, and waits for them, so the
// catalog the request is checked and routed by is complete. Concurrent
// requests share a refresh. An account still without a catalog is tried
// with the merged one, as before.
func (g *Gateway) refreshUnreported(ctx context.Context) {
	if g.refresh == nil {
		return
	}
	var waits []chan struct{}
	for _, account := range g.pool.Accounts() {
		st := account.Status()
		outOfRotation := st.Paused || st.Disabled != "" || st.Blocked != "" || st.Verify
		if outOfRotation || account.Models() != nil {
			continue
		}
		if done := g.startRefresh(account); done != nil {
			waits = append(waits, done)
		}
	}
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// startRefresh returns the account's running refresh, starting one unless
// the last left it without a catalog less than refreshRetry ago (nil).
func (g *Gateway) startRefresh(account *pool.Account) chan struct{} {
	name := account.Name()
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	if call := g.refreshes[name]; call != nil {
		if call.failedAt.IsZero() {
			return call.done
		}
		if time.Since(call.failedAt) < refreshRetry {
			return nil
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	g.refreshes[name] = call
	go func() {
		// Not the request's context: other requests may wait on it.
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		err := g.refresh(ctx, account)
		if err == nil && account.Models() == nil {
			err = errors.New("agy reported no model catalog")
		}
		g.refreshMu.Lock()
		if err != nil {
			g.log.Warn("account refresh before routing failed", "account", name, "err", err)
			call.failedAt = time.Now()
		} else {
			delete(g.refreshes, name)
		}
		close(call.done)
		g.refreshMu.Unlock()
	}()
	return call.done
}

func mergeEfforts(a, b []string) []string {
	out := slices.Clone(a)
	for _, tier := range b {
		if !slices.Contains(out, tier) {
			out = append(out, tier)
		}
	}
	slices.SortFunc(out, func(x, y string) int { return slices.Index(effortOrder, x) - slices.Index(effortOrder, y) })
	return out
}

func modelInfos(entries []modelEntry) []conv.ModelInfo {
	out := make([]conv.ModelInfo, len(entries))
	for i, e := range entries {
		slugs := []string{e.id}
		if len(e.efforts) > 0 {
			slugs = nil
			for _, tier := range e.efforts {
				slugs = append(slugs, e.id+"-"+tier)
			}
		}
		out[i] = conv.ModelInfo{ID: e.id, Name: e.name, Efforts: append([]string{}, e.efforts...), Slugs: slugs}
	}
	return out
}

// AccountModels lists the models agy last reported for an account, as the
// gateway serves them; nil before the account's first session or for an
// unknown account.
func (g *Gateway) AccountModels(account string) []conv.ModelInfo {
	a := g.pool.Account(account)
	if a == nil {
		return nil
	}
	models := a.Models()
	if models == nil {
		return nil
	}
	return modelInfos(entriesFromACP(models))
}

// modelChoice is a requested model as found in the catalog: the public id,
// the requested effort, and the tier a tier slug ("gemini-3.8-flash-high")
// pins, "" when the effort picks it.
type modelChoice struct {
	id, effort, pinned string
}

// lookupModel finds a public model id, or a tier slug, in entries.
func lookupModel(entries []modelEntry, model, effort string) (modelChoice, bool) {
	for _, e := range entries {
		if e.id == model {
			return modelChoice{id: e.id, effort: effort}, true
		}
	}
	if m := tierPattern.FindStringSubmatch(model); m != nil {
		for _, e := range entries {
			if e.id == m[1] && slices.Contains(e.efforts, m[2]) {
				return modelChoice{id: e.id, effort: effort, pinned: m[2]}, true
			}
		}
	}
	return modelChoice{}, false
}

// slugIn resolves the choice to the full ACP slug and the effort tier sent
// ("" for a model without tiers) in entries; ok is false when entries do
// not have the model or the pinned tier.
func (c modelChoice) slugIn(entries []modelEntry) (slug, tier string, ok bool) {
	i := slices.IndexFunc(entries, func(e modelEntry) bool { return e.id == c.id })
	if i < 0 {
		return "", "", false
	}
	efforts := entries[i].efforts
	switch {
	case len(efforts) == 0:
		return c.id, "", c.pinned == ""
	case c.pinned != "":
		return c.id + "-" + c.pinned, c.pinned, slices.Contains(efforts, c.pinned)
	}
	tier = pickEffort(c.effort, efforts)
	return c.id + "-" + tier, tier, true
}

// slugOn resolves the run's model on account by the catalog agy reported
// for it; an account that has not reported one is tried with what the
// accounts offer, and agy refuses a slug it lacks before any model call
// (verified on 1.5.3). ok is false when the account does not offer it.
func slugOn(account *pool.Account, r *run) (slug, tier string, ok bool) {
	models := account.Models()
	if models == nil {
		return r.model.slugIn(r.offered)
	}
	return r.model.slugIn(entriesFromACP(models))
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
