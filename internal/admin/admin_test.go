package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/control"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

func TestMain(m *testing.M) {
	if os.Getenv(agytest.EnvRun) == "1" {
		agytest.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const testPassword = "correct horse battery"

type env struct {
	t   *testing.T
	url string
	st  *store.Store
	ctl *control.Control
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(dir, "agyrelay.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bridge.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Config{Bin: os.Args[0], TurnsPerAccount: 1, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := control.LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	monitor := usage.NewMonitor(usage.MonitorConfig{Store: st, Pool: p, Profile: gateway.IdleProfile(), Log: log})
	ctl, err := control.New(context.Background(), control.Config{
		Store: st, Pool: p, Gateway: gw, Monitor: monitor, AgyBin: os.Args[0], Level: new(slog.LevelVar),
		AccountsDir: filepath.Join(dir, "accounts"), Settings: settings, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Store: st, Control: ctl, Pool: p, Monitor: monitor, Log: log}))
	t.Cleanup(func() {
		srv.Close()
		gw.Close()
		p.Close()
		b.Close()
		st.Close()
	})
	return &env{t: t, url: srv.URL, st: st, ctl: ctl}
}

type client struct {
	e      *env
	ip     string
	cookie string
	csrf   string
}

func (e *env) client(ip string) *client { return &client{e: e, ip: ip} }

type reply struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (c *client) do(method, path string, body any, extra map[string]string) reply {
	c.e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.e.url+path, reader)
	req.Header.Set("CF-Connecting-IP", c.ip)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if c.cookie != "" {
		req.Header.Set("Cookie", cookieName+"="+c.cookie)
	}
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}
	for k, v := range extra {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	json.Unmarshal(raw, &r.body)
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck.Value
		}
	}
	return r
}

func (c *client) login(password string) reply {
	c.e.t.Helper()
	r := c.do("POST", "/admin/api/login", map[string]string{"password": password}, nil)
	if csrf, ok := r.body["csrf"].(string); ok {
		c.csrf = csrf
	}
	return r
}

func TestLoginNeedsPassword(t *testing.T) {
	e := newEnv(t)
	if r := e.client("203.0.113.1").login("anything at all"); r.status != http.StatusServiceUnavailable {
		t.Fatalf("login without a password = %d %s", r.status, r.raw)
	}
}

func TestLoginCookieAndHeaders(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	r := c.login(testPassword)
	if r.status != http.StatusOK || c.csrf == "" {
		t.Fatalf("login = %d %s", r.status, r.raw)
	}
	set := r.header.Get("Set-Cookie")
	for _, want := range []string{cookieName + "=", "Path=/", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
	for name, want := range map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	} {
		if !strings.Contains(r.header.Get(name), want) {
			t.Errorf("%s = %q, want %q", name, r.header.Get(name), want)
		}
	}
	if r := c.do("GET", "/admin/api/session", nil, nil); r.status != http.StatusOK || r.body["csrf"] != c.csrf {
		t.Fatalf("session = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/logout", nil, nil); r.status != http.StatusOK {
		t.Fatalf("logout = %d", r.status)
	}
	if r := c.do("GET", "/admin/api/session", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d", r.status)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	attacker := e.client("198.51.100.7")
	for i := range loginLimits.PerClient {
		if r := attacker.login("wrong password " + string(rune('a'+i))); r.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, r.status)
		}
	}
	r := attacker.login(testPassword)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("locked-out login = %d %s", r.status, r.raw)
	}
	// The same /64 is the same client.
	if r := e.client("2001:db8::1").login("x"); r.status != http.StatusUnauthorized {
		t.Fatalf("v6 = %d", r.status)
	}
	if r := e.client("203.0.113.9").login(testPassword); r.status != http.StatusOK {
		t.Fatalf("another client = %d %s", r.status, r.raw)
	}
	failed, err := e.st.AuditLog(context.Background(), 0, 100)
	if err != nil || len(failed) < loginLimits.PerClient {
		t.Fatalf("audit = %v, %v", failed, err)
	}
}

func TestChangesNeedCSRFAndSameOrigin(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	body := map[string]any{"name": "alice", "note": "", "models": []string{}}

	if r := c.do("POST", "/admin/api/keys", body, map[string]string{csrfHeader: ""}); r.status != http.StatusForbidden {
		t.Fatalf("without CSRF = %d", r.status)
	}
	if r := c.do("POST", "/admin/api/keys", body, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site = %d", r.status)
	}
	if r := c.do("POST", "/admin/api/login", map[string]string{"password": testPassword}, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site login = %d", r.status)
	}
	r := c.do("POST", "/admin/api/keys", body, nil)
	token, _ := r.body["key"].(string)
	if r.status != http.StatusOK || !strings.HasPrefix(token, "sk-agyrelay-") {
		t.Fatalf("create key = %d %s", r.status, r.raw)
	}
	if caller, ok := e.ctl.Lookup(token); !ok || caller.Name != "alice" {
		t.Fatalf("lookup = %+v %v", caller, ok)
	}
	if r := c.do("POST", "/admin/api/keys", body, nil); r.status != http.StatusBadRequest {
		t.Fatalf("duplicate key = %d", r.status)
	}
	keys, _ := e.st.Keys(context.Background())
	if r := c.do("POST", "/admin/api/keys/"+itoa(keys[0].ID)+"/revoke", nil, nil); r.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", r.status, r.raw)
	}
	if _, ok := e.ctl.Lookup(token); ok {
		t.Fatal("revoked key still works")
	}
}

func TestPasswordChangeSignsOthersOut(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	me, other := e.client("203.0.113.1"), e.client("203.0.113.2")
	me.login(testPassword)
	other.login(testPassword)
	if r := me.do("POST", "/admin/api/password", map[string]string{"current": "nope", "new": "another long password"}, nil); r.status != http.StatusForbidden {
		t.Fatalf("wrong current = %d", r.status)
	}
	r := me.do("POST", "/admin/api/password", map[string]string{"current": testPassword, "new": "another long password"}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("change = %d %s", r.status, r.raw)
	}
	me.csrf = r.body["csrf"].(string)
	if r := me.do("GET", "/admin/api/settings", nil, nil); r.status != http.StatusOK {
		t.Fatalf("own session after change = %d", r.status)
	}
	if r := other.do("GET", "/admin/api/settings", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("other session after change = %d", r.status)
	}
	if r := e.client("203.0.113.3").login(testPassword); r.status != http.StatusUnauthorized {
		t.Fatalf("old password = %d", r.status)
	}
}

func TestIdleSessionExpires(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	sessions, _ := e.st.AdminSessions(context.Background())
	if err := e.st.TouchAdminSession(context.Background(), sessions[0].TokenHash, time.Now().Add(-sessionIdle-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := c.do("GET", "/admin/api/settings", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("idle session = %d", r.status)
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	bad := map[string]any{"turns_per_account": 0, "snapshot_ttl": "24h", "log_level": "info"}
	if r := c.do("PUT", "/admin/api/settings", bad, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad settings = %d %s", r.status, r.raw)
	}
	good := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "log_level": "debug"}
	if r := c.do("PUT", "/admin/api/settings", good, nil); r.status != http.StatusOK || r.body["turns_per_account"] != float64(3) {
		t.Fatalf("good settings = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings(); got.SnapshotTTL != "2h" {
		t.Fatalf("applied = %+v", got)
	}
}

func TestAccountSignIn(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	if r := c.do("POST", "/admin/api/accounts", map[string]any{"name": "../x", "env": []string{}}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad name = %d", r.status)
	}
	add := map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1", "HTTPS_PROXY=http://user:secret@proxy"}}
	if r := c.do("POST", "/admin/api/accounts", add, nil); r.status != http.StatusOK {
		t.Fatalf("add = %d %s", r.status, r.raw)
	}

	r := c.do("POST", "/admin/api/accounts/main/signin", nil, nil)
	if r.status != http.StatusOK || !strings.HasPrefix(r.body["url"].(string), "https://accounts.google.com/o/oauth2/") {
		t.Fatalf("start sign-in = %d %s", r.status, r.raw)
	}
	r = c.do("POST", "/admin/api/accounts/main/signin/code", map[string]string{"code": "4/wrong"}, nil)
	if r.status != http.StatusBadRequest || !strings.Contains(r.raw, "invalid_grant") {
		t.Fatalf("wrong code = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/accounts/main/signin/code", map[string]string{"code": agytest.LoginCode}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("code without a pending sign-in = %d", r.status)
	}
	c.do("POST", "/admin/api/accounts/main/signin", nil, nil)
	r = c.do("POST", "/admin/api/accounts/main/signin/code", map[string]string{"code": agytest.LoginCode}, nil)
	if r.status != http.StatusOK || r.body["email"] != agytest.LoginEmail {
		t.Fatalf("sign-in = %d %s", r.status, r.raw)
	}

	// The first quota sample runs in the background after the sign-in.
	deadline := time.Now().Add(15 * time.Second)
	for {
		r = c.do("GET", "/admin/api/accounts", nil, nil)
		if strings.Contains(r.raw, agytest.LoginEmail) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var accounts []accountView
	json.Unmarshal([]byte(r.raw), &accounts)
	if len(accounts) != 1 || accounts[0].User == nil || accounts[0].User.Plan != "Pro" || len(accounts[0].Groups) != 2 {
		t.Fatalf("accounts = %s", r.raw)
	}
	if strings.Contains(r.raw, "secret") || !strings.Contains(r.raw, "HTTPS_PROXY") {
		t.Fatalf("env values leak or names are missing: %s", r.raw)
	}

	if r := c.do("POST", "/admin/api/accounts/main/pause", nil, nil); r.status != http.StatusOK {
		t.Fatalf("pause = %d", r.status)
	}
	if err := e.ctl.Settings(); err.TurnsPerAccount == 0 {
		t.Fatal("settings lost")
	}
	if r := c.do("DELETE", "/admin/api/accounts/main", nil, nil); r.status != http.StatusOK {
		t.Fatalf("remove = %d", r.status)
	}
	if r := c.do("GET", "/admin/api/accounts", nil, nil); r.raw != "[]\n" {
		t.Fatalf("after remove = %s", r.raw)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
