package admin

import (
	"cmp"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agyrelay/internal/control"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

// settingsView is the settings with their defaults, for the UI's resets.
type settingsView struct {
	control.Settings
	Defaults control.Settings `json:"defaults"`
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settingsView{Settings: h.control.Settings(), Defaults: control.DefaultSettings()})
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request) {
	var s control.Settings
	if !decodeJSON(w, r, &s) {
		return
	}
	if err := h.control.UpdateSettings(r.Context(), s); err != nil {
		h.fail(w, "update settings", err)
		return
	}
	h.audit(r, "settings_changed", "")
	h.getSettings(w, r)
}

type modelView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
	// Upstream is the model's record in the upstream catalog, on account
	// views; nil when the catalog has none.
	Upstream map[string]any `json:"upstream,omitempty"`
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	out := []modelView{}
	for _, m := range h.control.Models() {
		out = append(out, modelView{ID: m.ID, Name: m.Name, Efforts: m.Efforts})
	}
	writeJSON(w, http.StatusOK, out)
}

type keyView struct {
	Hash         string   `json:"hash"`
	Name         string   `json:"name"`
	Note         string   `json:"note"`
	Models       []string `json:"models"`
	MaxWorkers   *int     `json:"max_workers"`
	EvictWaiting bool     `json:"evict_waiting"`
	// LimitTotal, Limits, and Spent are list-price USD; Limits and Spent
	// are by quota group.
	LimitTotal *float64                `json:"limit_total"`
	Limits     map[string]float64      `json:"limits"`
	Spent      map[string]float64      `json:"spent"`
	Quota      map[string]keyQuotaView `json:"quota"`
	Totals     keyTotalsView           `json:"totals"`
	CreatedAt  time.Time               `json:"created_at"`
	RevokedAt  *time.Time              `json:"revoked_at"`
}

// keyQuotaView totals a key's attributed weekly-quota shares in one quota
// group since the key was created, in all and by account.
type keyQuotaView struct {
	Used     float64            `json:"used"`
	Accounts []accountQuotaView `json:"accounts"`
}

type accountQuotaView struct {
	Account string  `json:"account"`
	Used    float64 `json:"used"`
}

// keyQuotasByHash groups attributed quota by key hash and quota group,
// keeping the query order (largest account share first).
func keyQuotasByHash(quotas []store.KeyQuota) map[string]map[string]keyQuotaView {
	out := map[string]map[string]keyQuotaView{}
	for _, q := range quotas {
		byGroup := out[q.KeyHash]
		if byGroup == nil {
			byGroup = map[string]keyQuotaView{}
			out[q.KeyHash] = byGroup
		}
		v := byGroup[q.Group]
		v.Used += q.Used
		v.Accounts = append(v.Accounts, accountQuotaView{Account: cmp.Or(q.AccountName, q.Account), Used: q.Used})
		byGroup[q.Group] = v
	}
	return out
}

// keyTotalsView counts a key's successful requests and their tokens since
// the key was created, in all and by model.
type keyTotalsView struct {
	Requests int64            `json:"requests"`
	Input    int64            `json:"input"`
	Output   int64            `json:"output"`
	Models   []modelTotalView `json:"models"`
}

type modelTotalView struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
}

// keyTotalsByHash groups the totals by key hash, keeping their order.
func keyTotalsByHash(totals []store.KeyTotal) map[string]keyTotalsView {
	out := map[string]keyTotalsView{}
	for _, m := range totals {
		v := out[m.KeyHash]
		v.Requests += m.Requests
		v.Input += m.Input
		v.Output += m.Output
		v.Models = append(v.Models, modelTotalView{Model: m.Model, Requests: m.Requests, Input: m.Input, Output: m.Output})
		out[m.KeyHash] = v
	}
	return out
}

type keysView struct {
	Keys []keyView `json:"keys"`
	// Groups lists the quota groups a limit can name.
	Groups []string `json:"groups"`
}

func buildKeyView(k store.Key, spent map[string]float64, totals keyTotalsView, quota map[string]keyQuotaView) keyView {
	v := keyView{
		Hash: k.Hash, Name: k.Name, Note: k.Note, Models: k.Models, MaxWorkers: k.MaxWorkers, EvictWaiting: k.EvictWaiting,
		LimitTotal: k.LimitTotal, Limits: k.Limits, Spent: spent, Quota: quota, Totals: totals, CreatedAt: k.CreatedAt,
	}
	if v.Models == nil {
		v.Models = []string{}
	}
	if v.Limits == nil {
		v.Limits = map[string]float64{}
	}
	if v.Spent == nil {
		v.Spent = map[string]float64{}
	}
	if v.Quota == nil {
		v.Quota = map[string]keyQuotaView{}
	}
	if v.Totals.Models == nil {
		v.Totals.Models = []modelTotalView{}
	}
	if !k.RevokedAt.IsZero() {
		revoked := k.RevokedAt
		v.RevokedAt = &revoked
	}
	return v
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.control.Keys(r.Context())
	if err != nil {
		h.internalError(w, "list keys", err)
		return
	}
	spends, err := h.control.KeySpends(r.Context())
	if err != nil {
		h.internalError(w, "key spend", err)
		return
	}
	totals, err := h.control.KeyTotals(r.Context())
	if err != nil {
		h.internalError(w, "key totals", err)
		return
	}
	quotas, err := h.control.KeyQuotas(r.Context())
	if err != nil {
		h.internalError(w, "key quota", err)
		return
	}
	byHash := keyTotalsByHash(totals)
	quotaByHash := keyQuotasByHash(quotas)
	out := keysView{Keys: []keyView{}, Groups: usage.Groups}
	for _, k := range keys {
		out.Keys = append(out.Keys, buildKeyView(k, spends[k.Hash], byHash[k.Hash], quotaByHash[k.Hash]))
	}
	writeJSON(w, http.StatusOK, out)
}

type userKeyView struct {
	Key    keyView  `json:"key"`
	Groups []string `json:"groups"`
}

// userKey serves the authenticated key's metadata on /user/api/key, omitting
// the admin note.
func (h *Handler) userKey(w http.ResponseWriter, r *http.Request) {
	k, _ := currentUserKey(r)
	spent, err := h.control.KeySpend(r.Context(), k.Hash)
	if err != nil {
		h.internalError(w, "key spend", err)
		return
	}
	totals, err := h.control.KeyTotalsFor(r.Context(), k.Hash)
	if err != nil {
		h.internalError(w, "key totals", err)
		return
	}
	quotas, err := h.control.KeyQuotaFor(r.Context(), k.Hash)
	if err != nil {
		h.internalError(w, "key quota", err)
		return
	}
	byHash := keyTotalsByHash(totals)
	quotaByHash := keyQuotasByHash(quotas)
	v := buildKeyView(k, spent, byHash[k.Hash], quotaByHash[k.Hash])
	v.Note = ""
	writeJSON(w, http.StatusOK, userKeyView{Key: v, Groups: usage.Groups})
}

type keyRequest struct {
	Name         string             `json:"name"`
	Note         string             `json:"note"`
	Models       []string           `json:"models"`
	MaxWorkers   *int               `json:"max_workers"`
	EvictWaiting bool               `json:"evict_waiting"`
	LimitTotal   *float64           `json:"limit_total"`
	Limits       map[string]float64 `json:"limits"`
}

func (k keyRequest) update() store.KeyUpdate {
	return store.KeyUpdate{
		Name: k.Name, Note: k.Note, Models: k.Models, MaxWorkers: k.MaxWorkers, EvictWaiting: k.EvictWaiting, LimitTotal: k.LimitTotal, Limits: k.Limits,
	}
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	token, hash, err := h.control.CreateKey(r.Context(), req.update())
	if err != nil {
		h.fail(w, "create key", err)
		return
	}
	h.audit(r, "key_created", name+" "+hash)
	writeJSON(w, http.StatusOK, map[string]string{"key": token})
}

// updateKey and the other key routes name the key by its hash; an unknown
// hash is not found.
func (h *Handler) updateKey(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	var req keyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	next := strings.TrimSpace(req.Name)
	oldName, err := h.control.UpdateKey(r.Context(), hash, req.update())
	if err != nil {
		h.fail(w, "update key", err)
		return
	}
	detail := hash
	if oldName != next {
		detail = oldName + " → " + next + " " + hash
	}
	h.audit(r, "key_updated", detail)
	writeJSON(w, http.StatusOK, map[string]any{})
}

type resetKeyUsageRequest struct {
	Group        string `json:"group"`
	IncludeStats bool   `json:"include_stats"`
}

func (h *Handler) resetKeyUsage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	var req resetKeyUsageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.control.ResetKeyUsage(r.Context(), hash, req.Group, req.IncludeStats); err != nil {
		h.fail(w, "reset key usage", err)
		return
	}
	scope := cmp.Or(req.Group, "all")
	detail := hash + " " + scope
	if req.IncludeStats {
		detail += " +stats"
	}
	h.audit(r, "key_usage_reset", detail)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if err := h.control.RevokeKey(r.Context(), hash); err != nil {
		h.fail(w, "revoke key", err)
		return
	}
	h.audit(r, "key_revoked", hash)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) restoreKey(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if err := h.control.RestoreKey(r.Context(), hash); err != nil {
		h.fail(w, "restore key", err)
		return
	}
	h.audit(r, "key_restored", hash)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	name, err := h.control.DeleteKey(r.Context(), hash)
	if err != nil {
		h.fail(w, "delete key", err)
		return
	}
	h.audit(r, "key_deleted", name+" "+hash)
	writeJSON(w, http.StatusOK, map[string]any{})
}

type adminSessionView struct {
	ID        string    `json:"id"`
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"created_at"`
	SeenAt    time.Time `json:"seen_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.store.AdminSessions(r.Context())
	if err != nil {
		h.internalError(w, "list sessions", err)
		return
	}
	current := currentSession(r).PublicID
	out := []adminSessionView{}
	for _, s := range sessions {
		out = append(out, adminSessionView{ID: s.PublicID, Current: s.PublicID == current, CreatedAt: s.CreatedAt, SeenAt: s.SeenAt, ExpiresAt: s.ExpiresAt, IP: s.IP, UserAgent: s.UserAgent})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.store.DeleteAdminSessionByPublicID(r.Context(), id); err != nil {
		h.fail(w, "revoke session", err)
		return
	}
	h.audit(r, "session_revoked", id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

type auditView struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	IP     string    `json:"ip"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

func (h *Handler) auditLog(w http.ResponseWriter, r *http.Request) {
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		if before, err = strconv.ParseInt(raw, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "before must be an entry id")
			return
		}
	}
	entries, err := h.store.AuditLog(r.Context(), before, 100)
	if err != nil {
		h.internalError(w, "read audit log", err)
		return
	}
	out := []auditView{}
	for _, e := range entries {
		out = append(out, auditView(e))
	}
	writeJSON(w, http.StatusOK, out)
}
