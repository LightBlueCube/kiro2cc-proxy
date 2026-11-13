package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/cloudcode"
	"agyrelay/internal/control"
	"agyrelay/internal/egress"
	"agyrelay/internal/gateway"
	"agyrelay/internal/guard"
	"agyrelay/internal/logbuf"
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
	// accounts holds the account HOME directories.
	accounts string
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
	settings, err := control.LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	recorder := usage.NewRecorder(st, settings.Prices, log)
	budget := usage.NewBudget(st, recorder)
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Budget: budget, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	monitor := usage.NewMonitor(usage.MonitorConfig{Store: st, Pool: p, Profile: gateway.IdleProfile(), Log: log})
	outbound, err := egress.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	cloudCode, err := cloudcode.Start(cloudcode.DefaultUpstream, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cloudCode.Close() })
	// Fake agy makes no outbound connections; let accounts route directly.
	settings.AllowDirect = true
	ctl, err := control.New(context.Background(), control.Config{
		Store: st, Pool: p, Gateway: gw, Monitor: monitor, Recorder: recorder, Budget: budget, Egress: outbound, CloudCode: cloudCode, AgyBin: os.Args[0], Level: new(slog.LevelVar),
		AccountsDir: filepath.Join(dir, "accounts"), Settings: settings, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := logbuf.NewBuffer(100)
	slog.New(logbuf.NewHandler(logs, slog.NewTextHandler(io.Discard, nil), slog.LevelDebug)).With("component", "test").Warn("hello", "account", "main")
	srv := httptest.NewServer(New(Config{Store: st, Control: ctl, Pool: p, Monitor: monitor, Logs: logs, Log: log, Proxies: guard.LoopbackProxies}))
	t.Cleanup(func() {
		srv.Close()
		outbound.Close()
		gw.Close()
		p.Close()
		b.Close()
		st.Close()
	})
	return &env{t: t, url: srv.URL, st: st, ctl: ctl, accounts: filepath.Join(dir, "accounts")}
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
	sum := sha256.Sum256([]byte(token))
	r = c.do("GET", "/admin/api/keys", nil, nil)
	var list keysView
	json.Unmarshal([]byte(r.raw), &list)
	if len(list.Keys) != 1 || list.Keys[0].Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("keys = %s, want the hash %x", r.raw, sum)
	}
	hash := list.Keys[0].Hash

	if r := c.do("DELETE", "/admin/api/keys/"+hash, nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("delete an active key = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/keys/"+hash+"/revoke", nil, nil); r.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", r.status, r.raw)
	}
	if _, ok := e.ctl.Lookup(token); ok {
		t.Fatal("revoked key still works")
	}
	if r := c.do("POST", "/admin/api/keys/"+hash+"/restore", nil, nil); r.status != http.StatusOK {
		t.Fatalf("restore = %d %s", r.status, r.raw)
	}
	if _, ok := e.ctl.Lookup(token); !ok {
		t.Fatal("restored key does not work")
	}
	if r := c.do("POST", "/admin/api/keys/"+hash+"/restore", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("restore a valid key = %d", r.status)
	}

	c.do("POST", "/admin/api/keys/"+hash+"/revoke", nil, nil)
	if r := c.do("DELETE", "/admin/api/keys/"+hash, nil, nil); r.status != http.StatusOK {
		t.Fatalf("delete = %d %s", r.status, r.raw)
	}
	if r := c.do("DELETE", "/admin/api/keys/"+hash, nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("delete again = %d", r.status)
	}
	// The name is free again; the new key is a different key.
	r = c.do("POST", "/admin/api/keys", body, nil)
	newToken, _ := r.body["key"].(string)
	if caller, ok := e.ctl.Lookup(newToken); r.status != http.StatusOK || !ok || caller.Name != "alice" || caller.KeyHash == hex.EncodeToString(sum[:]) {
		t.Fatalf("recreate = %d %s, lookup %+v", r.status, r.raw, caller)
	}
	if _, ok := e.ctl.Lookup(token); ok {
		t.Fatal("deleted key works")
	}
}

func TestKeyLimits(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	for _, bad := range []map[string]any{
		{"limits": map[string]float64{"nope": 1}},
		{"limits": map[string]float64{"gemini": -0.1}},
		{"limit_total": -1},
		{"limit_total": 5, "limits": map[string]float64{"gemini": 1}},
	} {
		bad["name"] = "bad"
		if r := c.do("POST", "/admin/api/keys", bad, nil); r.status != http.StatusBadRequest {
			t.Fatalf("limits %v = %d %s", bad, r.status, r.raw)
		}
	}
	r := c.do("POST", "/admin/api/keys", map[string]any{"name": "alice", "limits": map[string]float64{"claude-gpt": 30}}, nil)
	token, _ := r.body["key"].(string)
	if caller, ok := e.ctl.Lookup(token); !ok || caller.Limits["claude-gpt"] != 30 || len(caller.Limits) != 1 || caller.LimitTotal != nil {
		t.Fatalf("lookup = %+v %v (%s)", caller, ok, r.raw)
	}

	r = c.do("GET", "/admin/api/keys", nil, nil)
	var list keysView
	json.Unmarshal([]byte(r.raw), &list)
	if len(list.Keys) != 1 || list.Keys[0].Limits["claude-gpt"] != 30 || list.Keys[0].LimitTotal != nil || len(list.Groups) != 3 {
		t.Fatalf("keys = %s", r.raw)
	}

	// Switching to a total limit drops the group limits.
	hash := list.Keys[0].Hash
	if r := c.do("PATCH", "/admin/api/keys/"+hash, map[string]any{"note": "n", "limit_total": 20, "limits": map[string]float64{}}, nil); r.status != http.StatusOK {
		t.Fatalf("update = %d %s", r.status, r.raw)
	}
	if caller, _ := e.ctl.Lookup(token); len(caller.Limits) != 0 || caller.LimitTotal == nil || *caller.LimitTotal != 20 {
		t.Fatalf("after switching to a total = %+v", caller)
	}
	if r := c.do("PATCH", "/admin/api/keys/"+hash, map[string]any{"note": "n"}, nil); r.status != http.StatusOK {
		t.Fatalf("clear = %d %s", r.status, r.raw)
	}
	if caller, _ := e.ctl.Lookup(token); len(caller.Limits) != 0 || caller.LimitTotal != nil {
		t.Fatalf("limits after clearing = %+v", caller)
	}
}

func TestAccountProxy(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	for _, proxy := range []string{"socks5://127.0.0.1:7891", "http://127.0.0.1", "127.0.0.1:7890", "http://127.0.0.1:7890/path"} {
		if r := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "proxy": proxy}, nil); r.status != http.StatusBadRequest {
			t.Fatalf("proxy %q = %d %s", proxy, r.status, r.raw)
		}
	}
	if r := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "proxy": " http://u:secret@127.0.0.1:7890 "}, nil); r.status != http.StatusOK {
		t.Fatalf("add = %d %s", r.status, r.raw)
	}
	proxyOf := func() string {
		t.Helper()
		var accounts []accountView
		json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts)
		if len(accounts) != 1 {
			t.Fatalf("accounts = %+v", accounts)
		}
		return accounts[0].Proxy
	}
	if got := proxyOf(); got != "http://u:xxxxx@127.0.0.1:7890" {
		t.Fatalf("proxy = %q", got)
	}
	// Saving the masked value back keeps the password.
	if r := c.do("PUT", "/admin/api/accounts/main/proxy", map[string]string{"proxy": "http://u:xxxxx@127.0.0.1:7890"}, nil); r.status != http.StatusOK {
		t.Fatalf("resave = %d %s", r.status, r.raw)
	}
	if records, _ := e.ctl.AccountRecords(context.Background()); records["main"].Proxy != "http://u:secret@127.0.0.1:7890" {
		t.Fatalf("stored proxy = %q", records["main"].Proxy)
	}
	if r := c.do("PUT", "/admin/api/accounts/main/proxy", map[string]string{"proxy": "http://127.0.0.1:7891"}, nil); r.status != http.StatusOK {
		t.Fatalf("set = %d %s", r.status, r.raw)
	}
	if got := proxyOf(); got != "http://127.0.0.1:7891" {
		t.Fatalf("proxy = %q", got)
	}
	if r := c.do("PUT", "/admin/api/accounts/main/proxy", map[string]string{"proxy": ""}, nil); r.status != http.StatusOK || proxyOf() != "" {
		t.Fatalf("clear = %d %s", r.status, r.raw)
	}

	// Without its own proxy the account follows the settings: direct when
	// allowed, the fallback proxy, or blocked.
	routeOf := func() (string, string) {
		t.Helper()
		var accounts []accountView
		json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts)
		return accounts[0].Route, accounts[0].Blocked
	}
	if route, blocked := routeOf(); route != "direct" || blocked != "" {
		t.Fatalf("allowed direct: route %q, blocked %q", route, blocked)
	}
	settings := func(fallback string, direct bool) reply {
		t.Helper()
		s := map[string]any{"turns_per_account": 1, "snapshot_ttl": "24h", "session_idle_close": "5m", "log_level": "info", "retention_days": 90,
			"prices": []any{}, "fallback_proxy": fallback, "allow_direct": direct}
		return c.do("PUT", "/admin/api/settings", s, nil)
	}
	if r := settings("socks5://127.0.0.1:1", false); r.status != http.StatusBadRequest {
		t.Fatalf("bad fallback = %d %s", r.status, r.raw)
	}
	if r := settings("", false); r.status != http.StatusOK {
		t.Fatalf("no route = %d %s", r.status, r.raw)
	}
	if route, blocked := routeOf(); route != "refused" || !strings.Contains(blocked, "no proxy") {
		t.Fatalf("no route: route %q, blocked %q", route, blocked)
	}
	if r := c.do("POST", "/admin/api/accounts/main/signin", nil, nil); r.status != http.StatusBadRequest || !strings.Contains(r.raw, "no proxy") {
		t.Fatalf("sign-in without a route = %d %s", r.status, r.raw)
	}
	if r := settings("http://127.0.0.1:7899", false); r.status != http.StatusOK {
		t.Fatalf("fallback = %d %s", r.status, r.raw)
	}
	if route, blocked := routeOf(); route != "proxy http://127.0.0.1:7899" || blocked != "" {
		t.Fatalf("fallback: route %q, blocked %q", route, blocked)
	}
	if r := c.do("PUT", "/admin/api/accounts/nobody/proxy", map[string]string{"proxy": ""}, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown account = %d", r.status)
	}
}

func TestWorkers(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	if r := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}, nil); r.status != http.StatusOK {
		t.Fatalf("add = %d %s", r.status, r.raw)
	}
	workers := func() accountWorkersView {
		t.Helper()
		var v workersView
		json.Unmarshal([]byte(c.do("GET", "/admin/api/workers", nil, nil).raw), &v)
		if len(v.Accounts) != 1 {
			t.Fatalf("workers = %+v", v)
		}
		return v.Accounts[0]
	}
	if a := workers(); len(a.Procs) != 0 || a.Egress == nil || a.Egress.Route != "direct" {
		t.Fatalf("idle account = %+v", a)
	}
	accountModels := func() []modelView {
		t.Helper()
		var accounts []accountView
		json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts)
		if len(accounts) != 1 {
			t.Fatalf("accounts = %+v", accounts)
		}
		return accounts[0].Models
	}
	if m := accountModels(); m != nil {
		t.Fatalf("models before any session = %+v", m)
	}
	// A quota refresh starts agy, and its probe session reports the models.
	if r := c.do("POST", "/admin/api/accounts/main/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("refresh = %d %s", r.status, r.raw)
	}
	m := accountModels()
	if len(m) != 2 || m[0].ID != "gemini-3.8-flash" || !slices.Equal(m[0].Efforts, []string{"low", "high"}) || m[1].ID != "claude-opus-5-5" {
		t.Fatalf("models after refresh = %+v", m)
	}
	if a := workers(); len(a.Procs) != 1 || a.Procs[0].PID == 0 || a.Procs[0].RSSBytes == 0 {
		t.Fatalf("after refresh = %+v", a)
	}
	if r := c.do("POST", "/admin/api/workers/main/sessions/nope/cancel", nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("cancel unknown = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/workers/main/restart", nil, nil); r.status != http.StatusOK {
		t.Fatalf("restart = %d %s", r.status, r.raw)
	}
	if a := workers(); len(a.Procs) != 0 {
		t.Fatalf("after restart = %+v", a)
	}
	if r := c.do("POST", "/admin/api/workers/nobody/restart", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("restart unknown = %d", r.status)
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
	good := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "session_idle_close": "5m", "log_level": "debug", "retention_days": 30,
		"prices": []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}}}
	badPrice := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "session_idle_close": "5m", "log_level": "debug", "retention_days": 30,
		"prices": []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}, {"model": "m", "input": 2, "cached": 0.1, "output": 2}}}
	if r := c.do("PUT", "/admin/api/settings", badPrice, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad price = %d %s", r.status, r.raw)
	}
	if r := c.do("PUT", "/admin/api/settings", good, nil); r.status != http.StatusOK || r.body["turns_per_account"] != float64(3) {
		t.Fatalf("good settings = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings(); got.SnapshotTTL != "2h" || got.TurnsPerAccount != 3 {
		t.Fatalf("applied = %+v", got)
	}
}

func TestImportSignIn(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	add := map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}
	if r := c.do("POST", "/admin/api/accounts", add, nil); r.status != http.StatusOK {
		t.Fatalf("add = %d %s", r.status, r.raw)
	}
	tokenFile := filepath.Join(e.accounts, "main", ".gemini", "antigravity-cli", "antigravity-oauth-token")
	importToken := func(token string) reply {
		return c.do("POST", "/admin/api/accounts/main/signin/import", map[string]string{"token": token}, nil)
	}

	if r := importToken("ya29.access"); r.status != http.StatusBadRequest || !strings.Contains(r.raw, "access token") {
		t.Fatalf("access token = %d %s", r.status, r.raw)
	}
	if r := importToken("1//rejected"); r.status != http.StatusBadRequest || !strings.Contains(r.raw, "did not sign in") {
		t.Fatalf("rejected token = %d %s", r.status, r.raw)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("a rejected import left a sign-in behind: %v", err)
	}

	exported := `{"email":"x@example.com","token":{"access_token":"ya29.x","refresh_token":"` + agytest.RefreshToken + `"}}`
	r := importToken(exported)
	if r.status != http.StatusOK || r.body["email"] != agytest.LoginEmail {
		t.Fatalf("import = %d %s", r.status, r.raw)
	}
	info, err := os.Stat(tokenFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file = %v, %v", info, err)
	}
	installed, _ := os.ReadFile(tokenFile)

	// A rejected token puts the working sign-in back.
	if r := importToken("1//rejected"); r.status != http.StatusBadRequest {
		t.Fatalf("second rejected token = %d %s", r.status, r.raw)
	}
	if restored, _ := os.ReadFile(tokenFile); string(restored) != string(installed) {
		t.Fatalf("sign-in after a rejected import = %s, want %s", restored, installed)
	}

	entries, err := e.st.AuditLog(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, entry := range entries {
		actions = append(actions, entry.Action+" "+entry.Detail)
		if strings.Contains(entry.Detail, "1//") {
			t.Fatalf("audit entry holds a token: %+v", entry)
		}
	}
	if !slices.Contains(actions, "signin_imported main as "+agytest.LoginEmail) {
		t.Fatalf("audit = %v", actions)
	}

	// Removing the account deletes its sign-in with its HOME.
	if r := c.do("DELETE", "/admin/api/accounts/main", nil, nil); r.status != http.StatusOK {
		t.Fatalf("remove = %d %s", r.status, r.raw)
	}
	if _, err := os.Stat(filepath.Join(e.accounts, "main")); !os.IsNotExist(err) {
		t.Fatalf("HOME after removal: %v", err)
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

func TestLogsAndUsageNeedSession(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	anonymous := e.client("203.0.113.5")
	for _, path := range []string{"/admin/api/logs", "/admin/api/logs/stream", "/admin/api/requests", "/admin/api/usage?by=key", "/admin/api/accounts", "/admin/api/keys"} {
		if r := anonymous.do("GET", path, nil, nil); r.status != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", path, r.status)
		}
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	r := c.do("GET", "/admin/api/logs?level=warn&component=test&account=main&q=HELLO", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `"message":"hello"`) {
		t.Fatalf("logs = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/logs?level=error", nil, nil); r.raw != "[]\n" {
		t.Fatalf("error-level logs = %s", r.raw)
	}

	ctx := context.Background()
	cost := 0.5
	if err := e.st.AddRequest(ctx, store.Request{At: time.Now(), KeyHash: "hash-alice", KeyName: "alice", Account: "main", Model: "m", Status: "ok", Input: 100, Cached: 40, Output: 10, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	r = c.do("GET", "/admin/api/usage?by=day_key&tz=480", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `hash-alice","key_name":"alice","requests":1`) || !strings.Contains(r.raw, `"cost_usd":0.5`) {
		t.Fatalf("usage = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/requests?key=hash-alice&limit=10", nil, nil); r.status != http.StatusOK || !strings.Contains(r.raw, `"key":"alice","key_hash":"hash-alice"`) || !strings.Contains(r.raw, `"cached":40`) {
		t.Fatalf("requests = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/requests?key=alice&limit=10", nil, nil); r.raw != "[]\n" {
		t.Fatalf("requests filtered by name = %s, want none (the filter takes a hash)", r.raw)
	}
	if r := c.do("GET", "/admin/api/usage/filters?since=0", nil, nil); !strings.Contains(r.raw, `"key":[{"hash":"hash-alice","name":"alice"}]`) {
		t.Fatalf("filters = %s", r.raw)
	}
	if r := c.do("GET", "/admin/api/usage?by=nothing", nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad grouping = %d", r.status)
	}
}
