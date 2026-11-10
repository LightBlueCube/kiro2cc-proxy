package admin

import (
	"net/http"
	"strconv"
	"time"

	"agyrelay/internal/control"
)

type settingsView struct {
	control.Settings
	ProcessPerSession bool `json:"process_per_session"`
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settingsView{Settings: h.control.Settings(), ProcessPerSession: h.processPerSession})
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
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	out := []modelView{}
	for _, m := range h.control.Models() {
		out = append(out, modelView{ID: m.ID, Name: m.Name, Efforts: m.Efforts})
	}
	writeJSON(w, http.StatusOK, out)
}

type keyView struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Note      string     `json:"note"`
	Models    []string   `json:"models"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.control.Keys(r.Context())
	if err != nil {
		h.internalError(w, "list keys", err)
		return
	}
	out := []keyView{}
	for _, k := range keys {
		v := keyView{ID: k.ID, Name: k.Name, Note: k.Note, Models: k.Models, CreatedAt: k.CreatedAt}
		if v.Models == nil {
			v.Models = []string{}
		}
		if !k.RevokedAt.IsZero() {
			revoked := k.RevokedAt
			v.RevokedAt = &revoked
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type keyRequest struct {
	Name   string   `json:"name"`
	Note   string   `json:"note"`
	Models []string `json:"models"`
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := h.control.CreateKey(r.Context(), req.Name, req.Note, req.Models)
	if err != nil {
		h.fail(w, "create key", err)
		return
	}
	h.audit(r, "key_created", req.Name)
	writeJSON(w, http.StatusOK, map[string]string{"key": token})
}

func (h *Handler) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req struct {
		Note   string   `json:"note"`
		Models []string `json:"models"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.control.UpdateKey(r.Context(), id, req.Note, req.Models); err != nil {
		h.fail(w, "update key", err)
		return
	}
	h.audit(r, "key_updated", strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := h.control.RevokeKey(r.Context(), id); err != nil {
		h.fail(w, "revoke key", err)
		return
	}
	h.audit(r, "key_revoked", strconv.FormatInt(id, 10))
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
