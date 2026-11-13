package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/control"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

const (
	refreshTimeout = time.Minute
	weeklyWindow   = 7 * 24 * time.Hour
)

type accountView struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	EnvNames  []string  `json:"env_names"`
	// Proxy is the account's own proxy; Route is where its traffic goes
	// now ("proxy http://…", "direct", "refused").
	Proxy         string                   `json:"proxy"`
	Route         string                   `json:"route"`
	Paused        bool                     `json:"paused"`
	Priority      int                      `json:"priority"`
	AllowedModels []string                 `json:"allowed_models"`
	Disabled      string                   `json:"disabled"`
	Blocked       string                   `json:"blocked"`
	Verify        bool                     `json:"verify"`
	VerifyURL     string                   `json:"verify_url"`
	Cooldowns     map[string]time.Time     `json:"cooldowns"`
	Turns         int                      `json:"turns"`
	Procs         int                      `json:"procs"`
	Login         bool                     `json:"login_pending"`
	User          *agy.UserStatus          `json:"user"`
	Groups        []agy.QuotaGroup         `json:"quota"`
	PoolUsage     map[string]poolUsageView `json:"pool_usage"`
	SampledAt     *time.Time               `json:"sampled_at"`
	SampleErr     string                   `json:"sample_error"`
	// Models is nil until agy opens a session for the account.
	Models []modelView `json:"models"`
	// Unavailable is the agent models the account's upstream catalog lists
	// as disabled.
	Unavailable []modelView `json:"unavailable"`
}

// poolUsageView totals an account's tokens and list-price spend in one
// quota group over its current weekly cycle, in all and by model.
type poolUsageView struct {
	Since     time.Time          `json:"since"`
	Requests  int64              `json:"requests"`
	Input     int64              `json:"input"`
	Cached    int64              `json:"cached"`
	Output    int64              `json:"output"`
	Reasoning int64              `json:"reasoning"`
	CostUSD   float64            `json:"cost_usd"`
	Unpriced  int64              `json:"unpriced"`
	Models    []store.ModelUsage `json:"models"`
}

func poolWindowStart(g agy.QuotaGroup, now time.Time) (group string, since time.Time) {
	weekAgo := now.Add(-weeklyWindow)
	since = weekAgo
	for _, b := range g.Buckets {
		if group == "" {
			group = usage.BucketGroup(b.ID)
		}
		if b.Window == "weekly" && !b.ResetAt.IsZero() {
			if b.ResetAt.After(now) {
				since = b.ResetAt.Add(-weeklyWindow)
			} else if b.ResetAt.After(weekAgo) {
				since = b.ResetAt
			}
		}
	}
	return group, since
}

func (h *Handler) accountPoolUsage(ctx context.Context, account string, groups []agy.QuotaGroup, now time.Time) (map[string]poolUsageView, error) {
	out := map[string]poolUsageView{}
	for _, g := range groups {
		group, since := poolWindowStart(g, now)
		if group == "" {
			continue
		}
		models, err := h.store.AccountGroupUsage(ctx, account, group, since)
		if err != nil {
			return nil, err
		}
		if models == nil {
			models = []store.ModelUsage{}
		}
		view := poolUsageView{Since: since, Models: models}
		for _, m := range models {
			view.Requests += m.Requests
			view.Input += m.Input
			view.Cached += m.Cached
			view.Output += m.Output
			view.Reasoning += m.Reasoning
			view.CostUSD += m.CostUSD
			view.Unpriced += m.Unpriced
		}
		out[group] = view
	}
	return out, nil
}

// accountModels returns the account's models, each with its upstream
// record (its first tier's), and the models upstream lists as disabled.
func (h *Handler) accountModels(account string) (models, unavailable []modelView) {
	upstream := map[string]map[string]any{}
	unavailable = []modelView{}
	for _, m := range h.control.UpstreamModels(account) {
		if !m.Disabled {
			upstream[m.Slug] = m.Fields
			continue
		}
		name, _ := m.Fields["displayName"].(string)
		unavailable = append(unavailable, modelView{ID: m.Slug, Name: name, Efforts: []string{}, Upstream: m.Fields})
	}
	served := h.control.AccountModels(account)
	if served == nil {
		return nil, unavailable
	}
	models = []modelView{}
	for _, m := range served {
		v := modelView{ID: m.ID, Name: m.Name, Efforts: m.Efforts}
		for _, slug := range m.Slugs {
			if fields, ok := upstream[slug]; ok {
				v.Upstream = fields
				break
			}
		}
		models = append(models, v)
	}
	return models, unavailable
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	records, err := h.control.AccountRecords(r.Context())
	if err != nil {
		h.internalError(w, "list accounts", err)
		return
	}
	now := time.Now()
	out := []accountView{}
	for _, account := range h.pool.Accounts() {
		st := account.Status()
		allowed := account.AllowedModels()
		if allowed == nil {
			allowed = []string{}
		}
		v := accountView{
			Name: st.Name, Paused: st.Paused, Priority: account.Priority(), AllowedModels: allowed,
			Disabled: st.Disabled, Blocked: st.Blocked, Verify: st.Verify, VerifyURL: st.VerifyURL, Cooldowns: st.Cooldowns,
			Turns: st.Turns, Procs: st.Procs, Login: h.control.LoginPending(st.Name), EnvNames: []string{},
		}
		if rec, ok := records[st.Name]; ok {
			v.CreatedAt = rec.CreatedAt
			// Values may hold proxy credentials; only names are shown.
			for _, kv := range rec.Env {
				name, _, _ := strings.Cut(kv, "=")
				v.EnvNames = append(v.EnvNames, name)
			}
			v.Proxy = redactProxy(rec.Proxy)
		}
		if traffic, ok := h.control.Egress(st.Name); ok {
			v.Route = traffic.Route
		}
		if q, ok := h.monitor.Latest(st.Name); ok {
			if !q.At.IsZero() {
				user, at := q.User, q.At
				v.User, v.Groups, v.SampledAt = &user, q.Groups, &at
			}
			v.SampleErr = q.Err
		}
		v.PoolUsage, err = h.accountPoolUsage(r.Context(), st.Name, v.Groups, now)
		if err != nil {
			h.internalError(w, "account pool usage", err)
			return
		}
		v.Models, v.Unavailable = h.accountModels(st.Name)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// redactProxy hides a proxy password; stored proxies parse.
func redactProxy(proxy string) string {
	if proxy == "" {
		return ""
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return "(unreadable)"
	}
	return u.Redacted()
}

func (h *Handler) addAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string   `json:"name"`
		Env   []string `json:"env"`
		Proxy string   `json:"proxy"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := h.control.AddAccount(r.Context(), strings.TrimSpace(req.Name), req.Env, req.Proxy); err != nil {
		h.fail(w, "add account", err)
		return
	}
	h.audit(r, "account_added", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) setAccountProxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Proxy string `json:"proxy"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	records, err := h.control.AccountRecords(r.Context())
	if err != nil {
		h.internalError(w, "read account", err)
		return
	}
	// The page shows the proxy with its password masked; sending that back
	// unchanged keeps the stored password.
	if current := records[name].Proxy; current != "" && strings.TrimSpace(req.Proxy) == redactProxy(current) {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err := h.control.SetAccountProxy(r.Context(), name, req.Proxy); err != nil {
		h.fail(w, "set account proxy", err)
		return
	}
	h.audit(r, "account_proxy_set", name+" → "+cmp.Or(redactProxy(strings.TrimSpace(req.Proxy)), "none (fallback route)"))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) setAccountRouting(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Priority      int      `json:"priority"`
		AllowedModels []string `json:"allowed_models"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.control.SetAccountRouting(r.Context(), name, req.Priority, req.AllowedModels); err != nil {
		h.fail(w, "set account routing", err)
		return
	}
	models := "all"
	if account := h.pool.Account(name); account != nil && len(account.AllowedModels()) > 0 {
		models = strings.Join(account.AllowedModels(), ", ")
	}
	h.audit(r, "account_routing_set", fmt.Sprintf("%s → priority %d, models %s", name, req.Priority, models))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) removeAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.control.RemoveAccount(r.Context(), name); err != nil {
		h.fail(w, "remove account", err)
		return
	}
	h.audit(r, "account_removed", name)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) pauseAccount(paused bool) http.HandlerFunc {
	action := "account_resumed"
	if paused {
		action = "account_paused"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := h.control.SetAccountPaused(r.Context(), name, paused); err != nil {
			h.fail(w, action, err)
			return
		}
		h.audit(r, action, name)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (h *Handler) refreshAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), refreshTimeout)
	defer cancel()
	err := h.control.RefreshAccount(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case err != nil && h.awaitsVerification(name):
		writeError(w, http.StatusBadGateway, "Google asks for the account to be verified first: open the link on its card, "+
			"finish the check, then refresh. Quota refresh failed: "+err.Error())
	case err != nil:
		writeError(w, http.StatusBadGateway, "quota refresh failed: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (h *Handler) awaitsVerification(name string) bool {
	account := h.pool.Account(name)
	return account != nil && account.Status().Verify
}

func (h *Handler) startLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	url, err := h.control.StartLogin(r.Context(), name)
	var input *control.InputError
	switch {
	case errors.Is(err, store.ErrNotFound) || errors.As(err, &input):
		h.fail(w, "start sign-in", err)
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "could not start the sign-in: "+err.Error())
		return
	}
	h.audit(r, "signin_started", name)
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

func (h *Handler) submitLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	email, err := h.control.SubmitLogin(r.Context(), name, req.Code)
	if err != nil {
		h.audit(r, "signin_failed", name)
		h.fail(w, "finish sign-in", err)
		return
	}
	h.audit(r, "signin_finished", name+" as "+email)
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

// importSignIn signs the account in with a pasted refresh token. The token
// is never logged or audited.
func (h *Handler) importSignIn(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), refreshTimeout)
	defer cancel()
	imported, err := h.control.ImportSignIn(ctx, name, req.Token)
	if err != nil {
		h.audit(r, "signin_import_failed", name)
		h.fail(w, "import sign-in", err)
		return
	}
	if imported.Verify {
		h.audit(r, "signin_imported", name+", awaiting verification")
	} else {
		h.audit(r, "signin_imported", name+" as "+imported.Email)
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": imported.Email, "verify": imported.Verify})
}

func (h *Handler) cancelLogin(w http.ResponseWriter, r *http.Request) {
	h.control.CancelLogin(r.PathValue("name"))
	writeJSON(w, http.StatusOK, map[string]any{})
}
