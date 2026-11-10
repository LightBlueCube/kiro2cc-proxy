package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/crypto/argon2"

	"agyrelay/internal/guard"
	"agyrelay/internal/store"
)

const (
	passwordSetting = "admin_password"
	// MinPasswordLength keeps offline and online guessing out of reach.
	MinPasswordLength = 12
	maxPasswordLength = 1024

	cookieName = "__Host-agyrelay_admin"
	csrfHeader = "X-CSRF-Token"

	sessionIdle     = 12 * time.Hour
	sessionLifetime = 7 * 24 * time.Hour
	// touchEvery bounds writes: seen_at moves at most once per interval.
	touchEvery = time.Minute

	// Concurrent password hashes; each takes argon2Memory KiB.
	maxHashing  = 2
	hashingWait = 5 * time.Second
)

// argon2id parameters (RFC 9106 second recommended option, more passes).
const (
	argon2Time    = 3
	argon2Memory  = 64 * 1024
	argon2Threads = 2
	argon2KeyLen  = 32
	saltLen       = 16
)

// Wrong passwords: 5 per client per 15 minutes, then a lockout from 15
// minutes doubling to a day; 30 in total per 15 minutes, which keeps
// guessing a 12-character password hopeless even from many addresses.
var loginLimits = guard.Config{PerClient: 5, Global: 30, Window: 15 * time.Minute, Lockout: 15 * time.Minute, MaxLockout: 24 * time.Hour}

type passwordRecord struct {
	Alg       string `json:"alg"`
	Time      uint32 `json:"t"`
	Memory    uint32 `json:"m"`
	Threads   uint8  `json:"p"`
	Salt      []byte `json:"salt"`
	Hash      []byte `json:"hash"`
	ChangedAt int64  `json:"changed_at"`
}

func hashPassword(password string) passwordRecord {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	return passwordRecord{
		Alg: "argon2id", Time: argon2Time, Memory: argon2Memory, Threads: argon2Threads, Salt: salt,
		Hash:      argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen),
		ChangedAt: time.Now().Unix(),
	}
}

func (p passwordRecord) matches(password string) bool {
	got := argon2.IDKey([]byte(password), p.Salt, p.Time, p.Memory, p.Threads, uint32(len(p.Hash)))
	return subtle.ConstantTimeCompare(got, p.Hash) == 1
}

// ValidatePassword reports why password cannot be the admin password.
func ValidatePassword(password string) error {
	switch {
	case len([]rune(password)) < MinPasswordLength:
		return fmt.Errorf("the password needs at least %d characters", MinPasswordLength)
	case len(password) > maxPasswordLength:
		return fmt.Errorf("the password may have at most %d bytes", maxPasswordLength)
	}
	return nil
}

// SetPassword stores a new admin password and signs every admin out.
func SetPassword(ctx context.Context, st *store.Store, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if err := st.SetSetting(ctx, passwordSetting, hashPassword(password)); err != nil {
		return err
	}
	return st.DeleteAllAdminSessions(ctx)
}

func (h *Handler) passwordRecord(ctx context.Context) (passwordRecord, bool, error) {
	var rec passwordRecord
	ok, err := h.store.Setting(ctx, passwordSetting, &rec)
	if ok && rec.Alg != "argon2id" {
		return passwordRecord{}, false, fmt.Errorf("unknown password algorithm %q", rec.Alg)
	}
	return rec, ok, err
}

// checkPassword verifies password under the hashing limit.
func (h *Handler) checkPassword(ctx context.Context, rec passwordRecord, password string) (bool, error) {
	wait, cancel := context.WithTimeout(ctx, hashingWait)
	defer cancel()
	select {
	case h.hashing <- struct{}{}:
	case <-wait.Done():
		return false, errBusy
	}
	defer func() { <-h.hashing }()
	return rec.matches(password), nil
}

var errBusy = errors.New("busy")

// sameOrigin rejects requests a browser made on another site's behalf.
// Browsers send Sec-Fetch-Site; older ones only Origin. Clients without
// either are not browsers and carry no ambient cookie.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

type loginRequest struct {
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	ip := guard.ClientIP(r)
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if ok, wait := h.loginLimiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("too many wrong passwords; try again in %s", wait.Round(time.Second)))
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rec, ok, err := h.passwordRecord(r.Context())
	if err != nil {
		h.internalError(w, "read admin password", err)
		return
	}
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no admin password is set; run `agyrelay passwd` on the server")
		return
	}
	matched, err := h.checkPassword(r.Context(), rec, req.Password)
	if errors.Is(err, errBusy) {
		writeError(w, http.StatusServiceUnavailable, "busy; try again")
		return
	}
	if !matched {
		h.loginLimiter.Fail(ip)
		h.audit(r, "login_failed", "")
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	h.loginLimiter.Succeed(ip)
	csrf, err := h.startSession(w, r)
	if err != nil {
		h.internalError(w, "start admin session", err)
		return
	}
	h.audit(r, "login", "")
	writeJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
}

// startSession creates a session and sets its cookie; it returns the
// session's CSRF token.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request) (string, error) {
	token := randomToken()
	tokenHash := sha256.Sum256([]byte(token))
	now := time.Now()
	sess := store.AdminSession{
		TokenHash: tokenHash[:],
		PublicID:  randomToken()[:16],
		CSRF:      randomToken(),
		CreatedAt: now,
		SeenAt:    now,
		ExpiresAt: now.Add(sessionLifetime),
		IP:        guard.ClientIP(r).String(),
		UserAgent: truncate(r.UserAgent(), 200),
	}
	if err := h.store.PruneAdminSessions(r.Context(), now, now.Add(-sessionIdle)); err != nil {
		return "", err
	}
	if err := h.store.AddAdminSession(r.Context(), sess); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge: int(sessionLifetime.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return sess.CSRF, nil
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type sessionKey struct{}

// session returns the request's admin session, or ErrNotFound.
func (h *Handler) session(r *http.Request) (store.AdminSession, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return store.AdminSession{}, store.ErrNotFound
	}
	tokenHash := sha256.Sum256([]byte(cookie.Value))
	sess, err := h.store.AdminSession(r.Context(), tokenHash[:])
	if err != nil {
		return store.AdminSession{}, err
	}
	now := time.Now()
	if !now.Before(sess.ExpiresAt) || now.Sub(sess.SeenAt) > sessionIdle {
		if err := h.store.DeleteAdminSession(r.Context(), sess.TokenHash); err != nil {
			return store.AdminSession{}, err
		}
		return store.AdminSession{}, store.ErrNotFound
	}
	if now.Sub(sess.SeenAt) >= touchEvery {
		if err := h.store.TouchAdminSession(r.Context(), sess.TokenHash, now); err != nil {
			return store.AdminSession{}, err
		}
	}
	return sess, nil
}

// authed wraps an API handler: it requires a session, and for anything
// but GET a same-origin request carrying the session's CSRF token.
func (h *Handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := h.session(r)
		if errors.Is(err, store.ErrNotFound) {
			clearCookie(w)
			writeError(w, http.StatusUnauthorized, "signed out")
			return
		}
		if err != nil {
			h.internalError(w, "read admin session", err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get(csrfHeader)
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				writeError(w, http.StatusForbidden, "missing or wrong CSRF token; reload the page")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	}
}

func currentSession(r *http.Request) store.AdminSession {
	return r.Context().Value(sessionKey{}).(store.AdminSession)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteAdminSession(r.Context(), currentSession(r).TokenHash); err != nil {
		h.internalError(w, "delete admin session", err)
		return
	}
	clearCookie(w)
	h.audit(r, "logout", "")
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) sessionInfo(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	writeJSON(w, http.StatusOK, map[string]any{"csrf": sess.CSRF, "id": sess.PublicID, "expires_at": sess.ExpiresAt})
}

type passwordChange struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

// changePassword needs the current password; it signs out every other
// session and renews this one.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	ip := guard.ClientIP(r)
	if ok, wait := h.loginLimiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many wrong passwords")
		return
	}
	var req passwordChange
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := ValidatePassword(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, ok, err := h.passwordRecord(r.Context())
	if err != nil {
		h.internalError(w, "read admin password", err)
		return
	}
	if !ok {
		// Only a hand-edited database has sessions but no password.
		writeError(w, http.StatusConflict, "no admin password is set")
		return
	}
	matched, err := h.checkPassword(r.Context(), rec, req.Current)
	if errors.Is(err, errBusy) {
		writeError(w, http.StatusServiceUnavailable, "busy; try again")
		return
	}
	if !matched {
		h.loginLimiter.Fail(ip)
		h.audit(r, "password_change_failed", "")
		writeError(w, http.StatusForbidden, "the current password is wrong")
		return
	}
	if err := SetPassword(r.Context(), h.store, req.New); err != nil {
		h.internalError(w, "set admin password", err)
		return
	}
	csrf, err := h.startSession(w, r)
	if err != nil {
		h.internalError(w, "start admin session", err)
		return
	}
	h.audit(r, "password_changed", "all other sessions signed out")
	writeJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
}
