// Package admin serves the admin UI under /admin/: an embedded single-page
// app and its JSON API, behind a password session.
//
//	/admin/            static app (no data; public)
//	/admin/api/login   password → session cookie + CSRF token
//	/admin/api/...     session required; changes also need X-CSRF-Token
package admin

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"agyrelay/internal/control"
	"agyrelay/internal/guard"
	"agyrelay/internal/logbuf"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

//go:embed static
var staticFiles embed.FS

const maxBodyBytes = 64 << 10

type Config struct {
	Store   *store.Store
	Control *control.Control
	Pool    *pool.Pool
	Monitor *usage.Monitor
	Logs    *logbuf.Buffer
	Log     *slog.Logger
}

type Handler struct {
	store        *store.Store
	control      *control.Control
	pool         *pool.Pool
	monitor      *usage.Monitor
	logs         *logbuf.Buffer
	log          *slog.Logger
	loginLimiter *guard.Limiter
	hashing      chan struct{}
	mux          *http.ServeMux
}

func New(cfg Config) *Handler {
	h := &Handler{
		store:        cfg.Store,
		control:      cfg.Control,
		pool:         cfg.Pool,
		monitor:      cfg.Monitor,
		logs:         cfg.Logs,
		log:          cfg.Log,
		loginLimiter: guard.NewLimiter(loginLimits),
		hashing:      make(chan struct{}, maxHashing),
		mux:          http.NewServeMux(),
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	h.mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServerFS(static)))

	h.mux.HandleFunc("POST /admin/api/login", h.login)
	h.mux.HandleFunc("POST /admin/api/logout", h.authed(h.logout))
	h.mux.HandleFunc("GET /admin/api/session", h.authed(h.sessionInfo))
	h.mux.HandleFunc("POST /admin/api/password", h.authed(h.changePassword))
	h.mux.HandleFunc("GET /admin/api/sessions", h.authed(h.listSessions))
	h.mux.HandleFunc("DELETE /admin/api/sessions/{id}", h.authed(h.revokeSession))
	h.mux.HandleFunc("GET /admin/api/audit", h.authed(h.auditLog))

	h.mux.HandleFunc("GET /admin/api/settings", h.authed(h.getSettings))
	h.mux.HandleFunc("PUT /admin/api/settings", h.authed(h.putSettings))

	h.mux.HandleFunc("GET /admin/api/accounts", h.authed(h.listAccounts))
	h.mux.HandleFunc("POST /admin/api/accounts", h.authed(h.addAccount))
	h.mux.HandleFunc("DELETE /admin/api/accounts/{name}", h.authed(h.removeAccount))
	h.mux.HandleFunc("POST /admin/api/accounts/{name}/pause", h.authed(h.pauseAccount(true)))
	h.mux.HandleFunc("POST /admin/api/accounts/{name}/resume", h.authed(h.pauseAccount(false)))
	h.mux.HandleFunc("POST /admin/api/accounts/{name}/refresh", h.authed(h.refreshAccount))
	h.mux.HandleFunc("PUT /admin/api/accounts/{name}/proxy", h.authed(h.setAccountProxy))
	h.mux.HandleFunc("POST /admin/api/accounts/{name}/signin", h.authed(h.startLogin))
	h.mux.HandleFunc("POST /admin/api/accounts/{name}/signin/code", h.authed(h.submitLogin))
	h.mux.HandleFunc("DELETE /admin/api/accounts/{name}/signin", h.authed(h.cancelLogin))

	h.mux.HandleFunc("GET /admin/api/workers", h.authed(h.listWorkers))
	h.mux.HandleFunc("POST /admin/api/workers/{account}/sessions/{id}/cancel", h.authed(h.cancelTurn))
	h.mux.HandleFunc("POST /admin/api/workers/{account}/sessions/{id}/close", h.authed(h.closeSession))
	h.mux.HandleFunc("POST /admin/api/workers/{account}/restart", h.authed(h.restartAccount))

	h.mux.HandleFunc("GET /admin/api/logs", h.authed(h.listLogs))
	h.mux.HandleFunc("GET /admin/api/logs/stream", h.authed(h.streamLogs))

	h.mux.HandleFunc("GET /admin/api/requests", h.authed(h.listRequests))
	h.mux.HandleFunc("GET /admin/api/usage", h.authed(h.summarize))
	h.mux.HandleFunc("GET /admin/api/usage/filters", h.authed(h.filterValues))

	h.mux.HandleFunc("GET /admin/api/models", h.authed(h.listModels))
	h.mux.HandleFunc("GET /admin/api/keys", h.authed(h.listKeys))
	h.mux.HandleFunc("POST /admin/api/keys", h.authed(h.createKey))
	h.mux.HandleFunc("PATCH /admin/api/keys/{id}", h.authed(h.updateKey))
	h.mux.HandleFunc("POST /admin/api/keys/{id}/revoke", h.authed(h.revokeKey))
	h.mux.HandleFunc("POST /admin/api/keys/{id}/restore", h.authed(h.restoreKey))
	h.mux.HandleFunc("DELETE /admin/api/keys/{id}", h.authed(h.deleteKey))
	h.mux.HandleFunc("GET /admin/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	return h
}

// contentSecurityPolicy allows only the app's own files: no inline code,
// no framing, no foreign connections.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Cache-Control", "no-store")
	if guard.ViaHTTPS(r) {
		header.Set("Strict-Transport-Security", "max-age=31536000")
	}
	h.mux.ServeHTTP(w, r)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write error means the client left.
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// internalError logs err and answers 500 without details.
func (h *Handler) internalError(w http.ResponseWriter, what string, err error) {
	h.log.Error("admin request failed", "what", what, "err", err)
	writeError(w, http.StatusInternalServerError, what+" failed; see the server log")
}

// fail answers an error from the control plane: admin input errors and
// missing rows are shown, anything else is internal.
func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	var input *control.InputError
	switch {
	case errors.As(err, &input):
		writeError(w, http.StatusBadRequest, input.Message)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		h.internalError(w, what, err)
	}
}

// audit records an admin action; a failure is logged, not shown.
func (h *Handler) audit(r *http.Request, action, detail string) {
	entry := store.AuditEntry{At: time.Now(), IP: guard.ClientIP(r).String(), Action: action, Detail: detail}
	if err := h.store.Audit(r.Context(), entry); err != nil {
		h.log.Error("audit entry lost", "action", action, "err", err)
	}
	h.log.Info("admin action", "action", action, "detail", detail, "ip", entry.IP)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
