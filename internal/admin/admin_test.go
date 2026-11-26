package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/agy/agytest"
	"agyrelay/internal/bridge"
	"agyrelay/internal/cloudcode"
	"agyrelay/internal/control"
	"agyrelay/internal/egress"
	"agyrelay/internal/gateway"
	"agyrelay/internal/guard"
	"agyrelay/internal/logbuf"
	"agyrelay/internal/pool"
	"agyrelay/internal/promptflags"
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
	t      *testing.T
	url    string
	st     *store.Store
	ctl    *control.Control
	p      *pool.Pool
	egress *egress.Server
	// flags is the risk-control flags the gateway and control share.
	flags *promptflags.Flags
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
	recorder := usage.NewRecorder(st, settings.Prices, nil, log)
	budget := usage.NewBudget(st, recorder)
	flags, err := promptflags.New(context.Background(), st, log)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Recorder: recorder, Budget: budget, Flags: flags, Log: log})
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
		Store: st, Pool: p, Gateway: gw, Monitor: monitor, Recorder: recorder, Budget: budget, Flags: flags, Egress: outbound, CloudCode: cloudCode, AgyBin: os.Args[0], Level: new(slog.LevelVar),
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
	return &env{t: t, url: srv.URL, st: st, ctl: ctl, p: p, egress: outbound, flags: flags, accounts: filepath.Join(dir, "accounts")}
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
	if totals := list.Keys[0].Totals; totals.Requests != 0 || totals.Models == nil {
		t.Fatalf("totals of a new key = %+v", totals)
	}
	if list.Keys[0].Quota == nil || len(list.Keys[0].Quota) != 0 {
		t.Fatalf("quota of a new key = %+v", list.Keys[0].Quota)
	}

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	for _, req := range []store.Request{
		{At: now, KeyHash: hash, Account: "a1", QuotaGroup: "gemini", Model: "small", Status: store.StatusOK, Input: 10, Output: 1},
		{At: now, KeyHash: hash, Account: "a2", QuotaGroup: "gemini", Model: "big", Status: store.StatusOK, Input: 100, Output: 20},
		{At: now, KeyHash: hash, Account: "a1", QuotaGroup: "claude-gpt", Model: "small", Status: store.StatusOK, Input: 5, Output: 2},
	} {
		if err := e.st.AddRequest(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: hash}, 0, 10)
	if err != nil || len(reqs) != 3 {
		t.Fatalf("requests = %+v, %v", reqs, err)
	}
	// reqs are newest first: [a1/claude-gpt, a2/gemini, a1/gemini].
	if err := e.st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[2].ID: 0.12}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AttributeQuota(ctx, "a2", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[1].ID: 0.28}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AttributeQuota(ctx, "a1", "3p-weekly", now.Add(time.Minute), map[int64]float64{reqs[0].ID: 0.05}); err != nil {
		t.Fatal(err)
	}
	r = c.do("GET", "/admin/api/keys", nil, nil)
	json.Unmarshal([]byte(r.raw), &list)
	wantTotals := keyTotalsView{Requests: 3, Input: 115, Output: 23, Models: []modelTotalView{
		{Model: "big", Requests: 1, Input: 100, Output: 20},
		{Model: "small", Requests: 2, Input: 15, Output: 3},
	}}
	if !reflect.DeepEqual(list.Keys[0].Totals, wantTotals) {
		t.Fatalf("totals = %+v, want %+v", list.Keys[0].Totals, wantTotals)
	}
	wantQuota := map[string]keyQuotaView{
		"gemini":     {Used: 0.40, Accounts: []accountQuotaView{{Account: "a2", Used: 0.28}, {Account: "a1", Used: 0.12}}},
		"claude-gpt": {Used: 0.05, Accounts: []accountQuotaView{{Account: "a1", Used: 0.05}}},
	}
	if !reflect.DeepEqual(list.Keys[0].Quota, wantQuota) {
		t.Fatalf("quota = %+v, want %+v", list.Keys[0].Quota, wantQuota)
	}

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
		{"limits": map[string]float64{"argon": 1}},
		{"limits": map[string]float64{"gemini": -0.1}},
		{"limit_total": -1},
		{"limit_total": 5, "limits": map[string]float64{"gemini": 1}},
		{"max_workers": 0},
	} {
		bad["name"] = "bad"
		if r := c.do("POST", "/admin/api/keys", bad, nil); r.status != http.StatusBadRequest {
			t.Fatalf("limits %v = %d %s", bad, r.status, r.raw)
		}
	}
	r := c.do("POST", "/admin/api/keys", map[string]any{"name": "alice", "max_workers": 2, "evict_waiting": true, "limits": map[string]float64{"claude-gpt": 30}}, nil)
	token, _ := r.body["key"].(string)
	if caller, ok := e.ctl.Lookup(token); !ok || caller.MaxWorkers == nil || *caller.MaxWorkers != 2 || !caller.EvictWaiting || caller.Limits["claude-gpt"] != 30 || len(caller.Limits) != 1 || caller.LimitTotal != nil {
		t.Fatalf("lookup = %+v %v (%s)", caller, ok, r.raw)
	}

	r = c.do("GET", "/admin/api/keys", nil, nil)
	var list keysView
	json.Unmarshal([]byte(r.raw), &list)
	if len(list.Keys) != 1 || list.Keys[0].MaxWorkers == nil || *list.Keys[0].MaxWorkers != 2 || !list.Keys[0].EvictWaiting || list.Keys[0].Limits["claude-gpt"] != 30 || list.Keys[0].LimitTotal != nil || len(list.Groups) != 2 {
		t.Fatalf("keys = %s", r.raw)
	}
	userRes := c.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer " + token})
	var uv userKeyView
	if userRes.status != http.StatusOK || json.Unmarshal([]byte(userRes.raw), &uv) != nil || !uv.Key.EvictWaiting {
		t.Fatalf("/user/api/key = %d %s", userRes.status, userRes.raw)
	}

	// Switching to a total limit drops the group limits.
	hash := list.Keys[0].Hash
	if r := c.do("PATCH", "/admin/api/keys/"+hash, map[string]any{"name": "alice", "note": "n", "max_workers": 4, "evict_waiting": true, "limit_total": 20, "limits": map[string]float64{}}, nil); r.status != http.StatusOK {
		t.Fatalf("update = %d %s", r.status, r.raw)
	}
	if caller, _ := e.ctl.Lookup(token); len(caller.Limits) != 0 || caller.MaxWorkers == nil || *caller.MaxWorkers != 4 || !caller.EvictWaiting || caller.LimitTotal == nil || *caller.LimitTotal != 20 {
		t.Fatalf("after switching to a total = %+v", caller)
	}
	if r := c.do("PATCH", "/admin/api/keys/"+hash, map[string]any{"name": "alice", "note": "n"}, nil); r.status != http.StatusOK {
		t.Fatalf("clear = %d %s", r.status, r.raw)
	}
	if caller, _ := e.ctl.Lookup(token); len(caller.Limits) != 0 || caller.MaxWorkers != nil || caller.EvictWaiting || caller.LimitTotal != nil {
		t.Fatalf("limits after clearing = %+v", caller)
	}
}

func TestResetKeyUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := SetPassword(ctx, e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	create := c.do("POST", "/admin/api/keys", map[string]any{"name": "alice", "limit_total": 10}, nil)
	if create.status != http.StatusOK {
		t.Fatalf("create = %d %s", create.status, create.raw)
	}
	token, _ := create.body["key"].(string)
	getKey := func() keyView {
		t.Helper()
		r := c.do("GET", "/admin/api/keys", nil, nil)
		if r.status != http.StatusOK {
			t.Fatalf("list keys = %d %s", r.status, r.raw)
		}
		var list keysView
		if err := json.Unmarshal([]byte(r.raw), &list); err != nil || len(list.Keys) != 1 {
			t.Fatalf("keys = %s (%v)", r.raw, err)
		}
		return list.Keys[0]
	}
	hash := getKey().Hash

	for _, bad := range []map[string]any{
		{"group": "nope"},
		{"group": "argon"},
		{"unknown": true},
	} {
		if r := c.do("POST", "/admin/api/keys/"+hash+"/reset-usage", bad, nil); r.status != http.StatusBadRequest {
			t.Fatalf("reset %v = %d %s", bad, r.status, r.raw)
		}
	}
	if r := c.do("POST", "/admin/api/keys/missing/reset-usage", map[string]any{}, nil); r.status != http.StatusNotFound {
		t.Fatalf("reset missing = %d %s", r.status, r.raw)
	}

	now := time.Now().Truncate(time.Second)
	costG, costC := 4.0, 6.0
	for _, row := range []store.Request{
		{At: now, KeyHash: hash, KeyName: "alice", Account: "a1", AccountName: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK, Input: 100, Output: 20, CostUSD: &costG},
		{At: now.Add(time.Second), KeyHash: hash, KeyName: "alice", Account: "a1", AccountName: "main", QuotaGroup: "claude-gpt", Model: "claude-sonnet-4-6", Status: store.StatusOK, Input: 200, Output: 40, CostUSD: &costC},
	} {
		if err := e.st.AddRequest(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	reqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: hash}, 0, 10)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d, %v", len(reqs), err)
	}
	if err := e.st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[1].ID: 0.15}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AttributeQuota(ctx, "a1", "3p-weekly", now.Add(time.Minute), map[int64]float64{reqs[0].ID: 0.20}); err != nil {
		t.Fatal(err)
	}

	// 1. Reset gemini spend only; claude-gpt spend, all quota, and totals stay.
	if r := c.do("POST", "/admin/api/keys/"+hash+"/reset-usage", map[string]any{"group": "gemini", "include_stats": false}, nil); r.status != http.StatusOK {
		t.Fatalf("reset gemini = %d %s", r.status, r.raw)
	}
	kv := getKey()
	if !reflect.DeepEqual(kv.Spent, map[string]float64{"claude-gpt": 6.0}) {
		t.Fatalf("spent after gemini reset = %+v", kv.Spent)
	}
	if len(kv.Quota) != 2 || kv.Totals.Requests != 2 {
		t.Fatalf("quota/totals should remain intact: quota=%+v totals=%+v", kv.Quota, kv.Totals)
	}
	// /user/api/key sees the reset immediately too.
	userRes := c.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer " + token})
	if userRes.status != http.StatusOK {
		t.Fatalf("/user/api/key = %d %s", userRes.status, userRes.raw)
	}
	var uk userKeyView
	if err := json.Unmarshal([]byte(userRes.raw), &uk); err != nil || !reflect.DeepEqual(uk.Key.Spent, map[string]float64{"claude-gpt": 6.0}) {
		t.Fatalf("user key spent = %+v (%v)", uk.Key.Spent, err)
	}

	// 2. Reset gemini with stats (including an unsettled gemini request).
	if err := e.st.AddRequest(ctx, store.Request{At: now.Add(2 * time.Minute), KeyHash: hash, KeyName: "alice", Account: "a1", AccountName: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK, Input: 50, Output: 10, CostUSD: &costG}); err != nil {
		t.Fatal(err)
	}
	pending, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: hash}, 0, 1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, %v", len(pending), err)
	}
	if r := c.do("POST", "/admin/api/keys/"+hash+"/reset-usage", map[string]any{"group": "gemini", "include_stats": true}, nil); r.status != http.StatusOK {
		t.Fatalf("reset gemini+stats = %d %s", r.status, r.raw)
	}
	if err := e.st.AttributeQuota(ctx, "a1", "gemini-weekly", now.Add(10*time.Minute), map[int64]float64{pending[0].ID: 0.11}); err != nil {
		t.Fatal(err)
	}
	kv = getKey()
	if !reflect.DeepEqual(kv.Spent, map[string]float64{"claude-gpt": 6.0}) {
		t.Fatalf("spent after gemini+stats = %+v", kv.Spent)
	}
	if _, hasGemini := kv.Quota["gemini"]; hasGemini || kv.Quota["claude-gpt"].Used != 0.20 {
		t.Fatalf("quota after gemini+stats = %+v", kv.Quota)
	}
	if kv.Totals.Requests != 1 || len(kv.Totals.Models) != 1 || kv.Totals.Models[0].Model != "claude-sonnet-4-6" {
		t.Fatalf("totals after gemini+stats = %+v", kv.Totals)
	}

	// 3. Reset all spend only; remaining claude-gpt quota & totals stay.
	if r := c.do("POST", "/admin/api/keys/"+hash+"/reset-usage", map[string]any{"group": "", "include_stats": false}, nil); r.status != http.StatusOK {
		t.Fatalf("reset all = %d %s", r.status, r.raw)
	}
	kv = getKey()
	if len(kv.Spent) != 0 || kv.Quota["claude-gpt"].Used != 0.20 || kv.Totals.Requests != 1 {
		t.Fatalf("after reset all spend = %+v", kv)
	}

	// 4. Reset all with stats clears remaining quota & totals, preserving history.
	if r := c.do("POST", "/admin/api/keys/"+hash+"/reset-usage", map[string]any{"group": "", "include_stats": true}, nil); r.status != http.StatusOK {
		t.Fatalf("reset all+stats = %d %s", r.status, r.raw)
	}
	kv = getKey()
	if len(kv.Spent) != 0 || len(kv.Quota) != 0 || kv.Totals.Requests != 0 {
		t.Fatalf("after reset all+stats = %+v", kv)
	}
	if allReqs, err := e.st.Requests(ctx, store.RequestFilter{KeyHash: hash}, 0, 10); err != nil || len(allReqs) != 3 {
		t.Fatalf("request history after resets = %d, %v", len(allReqs), err)
	}

	entries, err := e.st.AuditLog(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var gotDetails []string
	for _, a := range entries {
		if a.Action == "key_usage_reset" {
			gotDetails = append(gotDetails, a.Detail)
		}
	}
	wantDetails := []string{
		hash + " all +stats",
		hash + " all",
		hash + " gemini +stats",
		hash + " gemini",
	}
	if !reflect.DeepEqual(gotDetails, wantDetails) {
		t.Fatalf("audit key_usage_reset = %v, want %v", gotDetails, wantDetails)
	}
}

func TestRenameKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := SetPassword(ctx, e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	r1 := c.do("POST", "/admin/api/keys", map[string]any{"name": "alice", "note": "orig"}, nil)
	r2 := c.do("POST", "/admin/api/keys", map[string]any{"name": "bob"}, nil)
	if r1.status != http.StatusOK || r2.status != http.StatusOK {
		t.Fatalf("create = %d / %d", r1.status, r2.status)
	}
	aliceToken, _ := r1.body["key"].(string)
	aliceCaller, ok := e.ctl.Lookup(aliceToken)
	if !ok {
		t.Fatal("alice lookup failed")
	}
	aliceHash := aliceCaller.KeyHash

	now := time.Now().Truncate(time.Millisecond)
	cost := 1.5
	if err := e.st.AddRequest(ctx, store.Request{
		At: now, KeyHash: aliceHash, KeyName: "alice", Account: "a1", AccountName: "main",
		QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK,
		Input: 100, Output: 20, CostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FlagPrompt(ctx, store.FlaggedPrompt{
		Fingerprint: "fp-rename", Text: "sys", LastAt: now, Until: now.Add(time.Hour),
		KeyHash: aliceHash, KeyName: "alice", Account: "a1", AccountName: "main", Model: "gemini-3.8-flash",
	}); err != nil {
		t.Fatal(err)
	}

	// Invalid names -> 400; duplicate -> 400; missing hash -> 404.
	for _, bad := range []string{"", "   ", "a/b", "a\nb", strings.Repeat("x", 65), "bob"} {
		if r := c.do("PATCH", "/admin/api/keys/"+aliceHash, map[string]any{"name": bad}, nil); r.status != http.StatusBadRequest {
			t.Fatalf("rename %q = %d %s, want 400", bad, r.status, r.raw)
		}
	}
	if r := c.do("PATCH", "/admin/api/keys/missing", map[string]any{"name": "next"}, nil); r.status != http.StatusNotFound {
		t.Fatalf("missing rename = %d %s, want 404", r.status, r.raw)
	}

	// Rename alice (with surrounding whitespace) and verify every surface reflects it.
	newName := "Alice (主力)"
	if r := c.do("PATCH", "/admin/api/keys/"+aliceHash, map[string]any{"name": "  " + newName + "  ", "note": "updated"}, nil); r.status != http.StatusOK {
		t.Fatalf("rename = %d %s", r.status, r.raw)
	}
	if caller, ok := e.ctl.Lookup(aliceToken); !ok || caller.KeyHash != aliceHash || caller.Name != newName {
		t.Fatalf("Lookup after rename = %+v %v", caller, ok)
	}
	var list keysView
	if err := json.Unmarshal([]byte(c.do("GET", "/admin/api/keys", nil, nil).raw), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 2 || list.Keys[0].Hash != aliceHash || list.Keys[0].Name != newName || list.Keys[0].Note != "updated" || list.Keys[0].Spent["gemini"] != 1.5 {
		t.Fatalf("keys after rename = %+v", list.Keys)
	}

	// User portal immediately serves the renamed key.
	user := e.client("203.0.113.2")
	auth := map[string]string{"Authorization": "Bearer " + aliceToken}
	var kv userKeyView
	if r := user.do("GET", "/user/api/key", nil, auth); r.status != http.StatusOK || json.Unmarshal([]byte(r.raw), &kv) != nil || kv.Key.Name != newName || kv.Key.Hash != aliceHash {
		t.Fatalf("user key = %d %s", r.status, r.raw)
	}

	// Historical requests, usage summary, filter menu, and flagged prompts reflect the new name.
	for _, path := range []string{
		"/admin/api/requests?key=" + aliceHash,
		"/admin/api/usage?by=key",
		"/admin/api/usage/filters?since=0",
		"/admin/api/flagged-prompts",
	} {
		r := c.do("GET", path, nil, nil)
		if r.status != http.StatusOK || !strings.Contains(r.raw, newName) || strings.Contains(r.raw, `"alice"`) {
			t.Fatalf("%s = %d %s", path, r.status, r.raw)
		}
	}

	// Saving again with the same name succeeds without self-conflict, and the old
	// name "alice" is free for a new key.
	if r := c.do("PATCH", "/admin/api/keys/"+aliceHash, map[string]any{"name": newName, "note": "again"}, nil); r.status != http.StatusOK {
		t.Fatalf("same-name update = %d %s", r.status, r.raw)
	}
	recreated := c.do("POST", "/admin/api/keys", map[string]any{"name": "alice"}, nil)
	if recreated.status != http.StatusOK {
		t.Fatalf("reuse old name = %d %s", recreated.status, recreated.raw)
	}
	newToken, _ := recreated.body["key"].(string)
	if caller, ok := e.ctl.Lookup(newToken); !ok || caller.Name != "alice" || caller.KeyHash == aliceHash {
		t.Fatalf("recreated caller = %+v %v", caller, ok)
	}

	// Audit log records the rename arrow when renamed, and plain hash otherwise.
	entries, err := e.st.AuditLog(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, a := range entries {
		if a.Action == "key_updated" {
			details = append(details, a.Detail)
		}
	}
	wantDetails := []string{
		aliceHash,
		"alice → " + newName + " " + aliceHash,
	}
	if !reflect.DeepEqual(details, wantDetails) {
		t.Fatalf("audit key_updated = %v, want %v", details, wantDetails)
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
	addReply := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "proxy": " http://u:secret@127.0.0.1:7890 "}, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)
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
	if r := c.do("PUT", "/admin/api/accounts/"+id+"/proxy", map[string]string{"proxy": "http://u:xxxxx@127.0.0.1:7890"}, nil); r.status != http.StatusOK {
		t.Fatalf("resave = %d %s", r.status, r.raw)
	}
	if records, _ := e.ctl.AccountRecords(context.Background()); records[id].Proxy != "http://u:secret@127.0.0.1:7890" {
		t.Fatalf("stored proxy = %q", records[id].Proxy)
	}
	if r := c.do("PUT", "/admin/api/accounts/"+id+"/proxy", map[string]string{"proxy": "http://127.0.0.1:7891"}, nil); r.status != http.StatusOK {
		t.Fatalf("set = %d %s", r.status, r.raw)
	}
	if got := proxyOf(); got != "http://127.0.0.1:7891" {
		t.Fatalf("proxy = %q", got)
	}
	if r := c.do("PUT", "/admin/api/accounts/"+id+"/proxy", map[string]string{"proxy": ""}, nil); r.status != http.StatusOK || proxyOf() != "" {
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
		s := map[string]any{"turns_per_account": 1, "snapshot_ttl": "24h", "session_idle_close": "5m", "park_ttl": "5m", "log_level": "info", "retention_days": 90, "capture_response_mib": 4, "flagged_prompt_retention_days": 30,
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
	if r := c.do("POST", "/admin/api/accounts/"+id+"/signin", nil, nil); r.status != http.StatusBadRequest || !strings.Contains(r.raw, "no proxy") {
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

	// Routing priority and allowed models persist and apply to the pool.
	if r := c.do("PUT", "/admin/api/accounts/"+id+"/routing", map[string]any{
		"priority":       7,
		"allowed_models": []string{" gemini-3.8-flash ", "gemini-3.8-flash", ""},
	}, nil); r.status != http.StatusOK {
		t.Fatalf("set routing = %d %s", r.status, r.raw)
	}
	var accounts []accountView
	json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts)
	if accounts[0].Priority != 7 || !slices.Equal(accounts[0].AllowedModels, []string{"gemini-3.8-flash"}) {
		t.Fatalf("account routing = %+v", accounts[0])
	}
	if records, _ := e.ctl.AccountRecords(context.Background()); records[id].Priority != 7 || !slices.Equal(records[id].Models, []string{"gemini-3.8-flash"}) {
		t.Fatalf("stored routing = %+v", records[id])
	}
	if r := c.do("PUT", "/admin/api/accounts/nobody/routing", map[string]any{"priority": 1}, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown account routing = %d", r.status)
	}
}

func TestWorkers(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	addReply := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)
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
	if r := c.do("POST", "/admin/api/accounts/"+id+"/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("refresh = %d %s", r.status, r.raw)
	}
	m := accountModels()
	if len(m) != 2 || m[0].ID != "gemini-3.8-flash" || !slices.Equal(m[0].Efforts, []string{"low", "high"}) || m[1].ID != "claude-opus-5-5" {
		t.Fatalf("models after refresh = %+v", m)
	}
	// Fake agy sets weekly reset_at to now + 72h, so the current week started 4d ago.
	ctx := context.Background()
	oldCost, curCost := 9.0, 1.25
	for _, req := range []store.Request{
		{At: time.Now().Add(-5 * 24 * time.Hour), Account: id, AccountName: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK, Input: 5000, Output: 500, CostUSD: &oldCost},
		{At: time.Now().Add(-2 * time.Hour), Account: id, AccountName: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK, Input: 1000, Cached: 400, Output: 100, CostUSD: &curCost},
		{At: time.Now().Add(-time.Hour), Account: id, AccountName: "main", QuotaGroup: "gemini", Model: "gemini-3.1-pro", Status: store.StatusOK, Input: 200, Output: 20},
	} {
		if err := e.st.AddRequest(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var refreshed []accountView
	if err := json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &refreshed); err != nil {
		t.Fatal(err)
	}
	gemPool := refreshed[0].PoolUsage["gemini"]
	if gemPool.Requests != 2 || gemPool.Input != 1200 || gemPool.Cached != 400 || gemPool.Output != 120 || gemPool.CostUSD != 1.25 || gemPool.Unpriced != 1 || len(gemPool.Models) != 2 {
		t.Fatalf("gemini pool_usage = %+v", gemPool)
	}
	if claudePool, ok := refreshed[0].PoolUsage["claude-gpt"]; !ok || claudePool.Requests != 0 || len(claudePool.Models) != 0 {
		t.Fatalf("claude-gpt pool_usage = %+v, ok=%v", claudePool, ok)
	}
	if a := workers(); len(a.Procs) != 1 || a.Procs[0].PID == 0 || a.Procs[0].RSSBytes == 0 {
		t.Fatalf("after refresh = %+v", a)
	}
	if r := c.do("POST", "/admin/api/workers/"+id+"/sessions/nope/cancel", nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("cancel unknown = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/workers/"+id+"/restart", nil, nil); r.status != http.StatusOK {
		t.Fatalf("restart = %d %s", r.status, r.raw)
	}
	if a := workers(); len(a.Procs) != 0 {
		t.Fatalf("after restart = %+v", a)
	}
	// Default egress rules reject play.googleapis.com and report it under workers.
	proxyURL, err := url.Parse(e.egress.Register(id))
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if _, err := hc.Get("https://play.googleapis.com/log"); err == nil {
		t.Fatal("play.googleapis.com succeeded through default egress rules")
	}
	if a := workers(); a.Egress == nil || a.Egress.Rejected != 1 || a.Egress.Refused != 0 || len(a.Egress.Hosts) != 1 || a.Egress.Hosts[0].Host != "play.googleapis.com" || a.Egress.Hosts[0].Rejected != 1 {
		t.Fatalf("workers egress after rule reject = %+v", a.Egress)
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
	good := map[string]any{"turns_per_account": 3, "max_workers": 10, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "10m", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14, "risk_control": false,
		"egress_rules": []string{"  DOMAIN-SUFFIX,telemetry.example.com,REJECT  ", ""},
		"prices":       []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}}}
	badPark := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "30s", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14,
		"prices": []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}}}
	if r := c.do("PUT", "/admin/api/settings", badPark, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad park_ttl = %d %s", r.status, r.raw)
	}
	badRule := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "5m", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14,
		"egress_rules": []string{"DOMAIN,play.googleapis.com,INVALID_ACTION"},
		"prices":       []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}}}
	if r := c.do("PUT", "/admin/api/settings", badRule, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad egress rule = %d %s", r.status, r.raw)
	}
	badPrice := map[string]any{"turns_per_account": 3, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "5m", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14,
		"prices": []map[string]any{{"model": "m", "input": 1, "cached": 0.1, "output": 2}, {"model": "m", "input": 2, "cached": 0.1, "output": 2}}}
	if r := c.do("PUT", "/admin/api/settings", badPrice, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad price = %d %s", r.status, r.raw)
	}
	if r := c.do("PUT", "/admin/api/settings", good, nil); r.status != http.StatusOK || r.body["turns_per_account"] != float64(3) || r.body["max_workers"] != float64(10) || r.body["park_ttl"] != "10m" {
		t.Fatalf("good settings = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings(); got.SnapshotTTL != "2h" || got.ParkTTL != "10m" || got.TurnsPerAccount != 3 || got.MaxWorkers != 10 || !slices.Equal(got.EgressRules, []string{"DOMAIN-SUFFIX,telemetry.example.com,REJECT"}) {
		t.Fatalf("applied = %+v", got)
	}
	// An update from an older tab that omits egress_rules keeps the active rules;
	// an explicit empty array clears them.
	omitRules := map[string]any{"turns_per_account": 3, "max_workers": 10, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "10m", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14}
	if r := c.do("PUT", "/admin/api/settings", omitRules, nil); r.status != http.StatusOK || !slices.Equal(e.ctl.Settings().EgressRules, []string{"DOMAIN-SUFFIX,telemetry.example.com,REJECT"}) {
		t.Fatalf("omitted egress_rules = %d %+v", r.status, e.ctl.Settings().EgressRules)
	}
	clearRules := map[string]any{"turns_per_account": 3, "max_workers": 10, "snapshot_ttl": "2h", "session_idle_close": "5m", "park_ttl": "10m", "log_level": "debug", "retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14, "egress_rules": []string{}}
	if r := c.do("PUT", "/admin/api/settings", clearRules, nil); r.status != http.StatusOK || len(e.ctl.Settings().EgressRules) != 0 {
		t.Fatalf("cleared egress_rules = %d %+v", r.status, e.ctl.Settings().EgressRules)
	}
	if got := e.ctl.Settings(); got.RiskControl || got.FlaggedPromptRetentionDays != 14 || e.flags.Enabled() {
		t.Fatalf("risk control = %v, %d days, flags enabled = %v; want it off with 14 days", got.RiskControl, got.FlaggedPromptRetentionDays, e.flags.Enabled())
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
	addReply := c.do("POST", "/admin/api/accounts", add, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)
	tokenFile := filepath.Join(e.accounts, id, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	importToken := func(token string) reply {
		return c.do("POST", "/admin/api/accounts/"+id+"/signin/import", map[string]string{"token": token}, nil)
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
	if got := e.p.Account(id).Status().Disabled; got != "" {
		t.Fatalf("a rejected import disabled the account whose sign-in it restored: %q", got)
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
	if r := c.do("DELETE", "/admin/api/accounts/"+id, nil, nil); r.status != http.StatusOK {
		t.Fatalf("remove = %d %s", r.status, r.raw)
	}
	if _, err := os.Stat(filepath.Join(e.accounts, id)); !os.IsNotExist(err) {
		t.Fatalf("HOME after removal: %v", err)
	}
}

func TestRefreshReenablesRecoveredAccount(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	add := map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}
	addReply := c.do("POST", "/admin/api/accounts", add, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)
	tokenDir := filepath.Join(e.accounts, id, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(tokenDir, "antigravity-oauth-token")
	valid := `{"token":{"refresh_token":"` + agytest.RefreshToken + `"},"auth_method":"consumer"}`
	if err := os.WriteFile(tokenFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := c.do("POST", "/admin/api/accounts/"+id+"/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("initial refresh = %d %s", r.status, r.raw)
	}

	// Simulate gateway failing session/new with Authentication required.
	e.p.Account(id).Disable(fmt.Errorf("%w (session/new: Authentication required)", agy.ErrAuthRequired))
	if err := os.WriteFile(tokenFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := c.do("POST", "/admin/api/accounts/"+id+"/refresh", nil, nil); r.status != http.StatusBadGateway {
		t.Fatalf("refresh with bad token = %d %s", r.status, r.raw)
	}
	var accounts []accountView
	if err := json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Disabled == "" || accounts[0].SampleErr == "" || accounts[0].User == nil {
		t.Fatalf("accounts while disabled = %+v", accounts)
	}

	if err := os.WriteFile(tokenFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := c.do("POST", "/admin/api/accounts/"+id+"/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("refresh after recovery = %d %s", r.status, r.raw)
	}
	accounts = nil
	if err := json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Disabled != "" || accounts[0].SampleErr != "" || accounts[0].User == nil || len(accounts[0].Groups) == 0 || len(accounts[0].Models) == 0 {
		t.Fatalf("accounts after recovery = %+v", accounts)
	}
	if err := e.p.Account(id).Available("gemini"); err != nil {
		t.Fatalf("account still unavailable after recovery: %v", err)
	}
}

func TestAgyLogs(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	addReply := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main"}, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)
	logDir := filepath.Join(e.accounts, id, ".gemini", "antigravity-cli", "log")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = "acp-worker-20261113_085132-999999999.log"
	text := "I1113 08:51:32.100000      46 server.go:313] started\n" +
		`I1113 08:51:32.578620     103 http_helpers.go:820] Failed to make code assist backend request: {"validation_url": "https://accounts.google.com/x"}` + "\n" +
		"E1113 08:51:33.000000     103 errorreport.go:273] PERMISSION_DENIED (code 403): Verify your account to continue.\n"
	if err := os.WriteFile(filepath.Join(logDir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	var listed struct {
		Files []agyLogFileView `json:"files"`
	}
	r := c.do("GET", "/admin/api/workers/"+id+"/logs", nil, nil)
	if json.Unmarshal([]byte(r.raw), &listed) != nil || len(listed.Files) != 1 || listed.Files[0].Kind != "worker" {
		t.Fatalf("list = %d %s", r.status, r.raw)
	}
	var found struct {
		Entries []agyLogEntryView `json:"entries"`
		Matched int               `json:"matched"`
	}
	r = c.do("GET", "/admin/api/workers/"+id+"/logs/entries?q=validation_url", nil, nil)
	if json.Unmarshal([]byte(r.raw), &found) != nil || found.Matched != 1 ||
		!strings.Contains(found.Entries[0].Text, "https://accounts.google.com/x") || found.Entries[0].File != name {
		t.Fatalf("search = %d %s", r.status, r.raw)
	}
	r = c.do("GET", "/admin/api/workers/"+id+"/logs/entries?level=E&file="+name, nil, nil)
	if json.Unmarshal([]byte(r.raw), &found) != nil || found.Matched != 1 || found.Entries[0].Level != "E" {
		t.Fatalf("errors only = %d %s", r.status, r.raw)
	}
	for _, query := range []string{"level=X", "limit=0", "file=../agyrelay.db"} {
		if r := c.do("GET", "/admin/api/workers/"+id+"/logs/entries?"+query, nil, nil); r.status != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", query, r.status, r.raw)
		}
	}
	if r := c.do("GET", "/admin/api/workers/nobody/logs", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown account = %d %s", r.status, r.raw)
	}
}

func TestAccountSignIn(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)
	for _, bad := range []string{"", "   ", "bad\x00name", strings.Repeat("a", 65)} {
		if r := c.do("POST", "/admin/api/accounts", map[string]any{"name": bad, "env": []string{}}, nil); r.status != http.StatusBadRequest {
			t.Fatalf("bad name %q = %d", bad, r.status)
		}
	}
	add := map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1", "HTTPS_PROXY=http://user:secret@proxy"}}
	addReply := c.do("POST", "/admin/api/accounts", add, nil)
	if addReply.status != http.StatusOK {
		t.Fatalf("add = %d %s", addReply.status, addReply.raw)
	}
	id := addReply.body["id"].(string)

	r := c.do("POST", "/admin/api/accounts/"+id+"/signin", nil, nil)
	if r.status != http.StatusOK || !strings.HasPrefix(r.body["url"].(string), "https://accounts.google.com/o/oauth2/") {
		t.Fatalf("start sign-in = %d %s", r.status, r.raw)
	}
	r = c.do("POST", "/admin/api/accounts/"+id+"/signin/code", map[string]string{"code": "4/wrong"}, nil)
	if r.status != http.StatusBadRequest || !strings.Contains(r.raw, "invalid_grant") {
		t.Fatalf("wrong code = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/accounts/"+id+"/signin/code", map[string]string{"code": agytest.LoginCode}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("code without a pending sign-in = %d", r.status)
	}
	c.do("POST", "/admin/api/accounts/"+id+"/signin", nil, nil)
	r = c.do("POST", "/admin/api/accounts/"+id+"/signin/code", map[string]string{"code": agytest.LoginCode}, nil)
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

	if r := c.do("POST", "/admin/api/accounts/"+id+"/pause", nil, nil); r.status != http.StatusOK {
		t.Fatalf("pause = %d", r.status)
	}
	const locReason = "FAILED_PRECONDITION (code 400): User location is not supported for the API use."
	e.p.Account(id).PauseWithReason(locReason)
	if r := c.do("GET", "/admin/api/accounts", nil, nil); r.status != http.StatusOK || !strings.Contains(r.raw, `"pause_reason":"`+locReason+`"`) {
		t.Fatalf("accounts with pause_reason = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/workers", nil, nil); r.status != http.StatusOK || !strings.Contains(r.raw, `"pause_reason":"`+locReason+`"`) {
		t.Fatalf("workers with pause_reason = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/accounts/"+id+"/resume", nil, nil); r.status != http.StatusOK {
		t.Fatalf("resume = %d", r.status)
	}
	if r := c.do("GET", "/admin/api/accounts", nil, nil); r.status != http.StatusOK || !strings.Contains(r.raw, `"paused":false,"pause_reason":""`) {
		t.Fatalf("accounts after resume = %d %s", r.status, r.raw)
	}
	if err := e.ctl.Settings(); err.TurnsPerAccount == 0 {
		t.Fatal("settings lost")
	}
	if r := c.do("DELETE", "/admin/api/accounts/"+id, nil, nil); r.status != http.StatusOK {
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
	at := time.Now().Truncate(time.Millisecond)
	if err := e.st.AddRequest(ctx, store.Request{At: at, KeyHash: "hash-alice", KeyName: "alice", Account: "main", Model: "m", Stream: true, Anchor: "ok", Status: "ok", Input: 100, Cached: 40, Output: 10, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	r = c.do("GET", "/admin/api/usage?by=day_key&tz=480", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `hash-alice","key_name":"alice","requests":1`) || !strings.Contains(r.raw, `"cost_usd":0.5`) {
		t.Fatalf("usage = %d %s", r.status, r.raw)
	}
	wantHour := at.UTC().Add(480 * time.Minute).Format("2006-01-02 15:00")
	r = c.do("GET", "/admin/api/usage?by=hour&tz=480", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `"group":"`+wantHour+`"`) {
		t.Fatalf("usage by hour = %d %s (want %s)", r.status, r.raw, wantHour)
	}
	r = c.do("GET", "/admin/api/usage?by=hour_key&tz=480", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `"group":"`+wantHour+`\u001fhash-alice","key_name":"alice","requests":1`) {
		t.Fatalf("usage by hour_key = %d %s (want %s)", r.status, r.raw, wantHour)
	}
	if r := c.do("GET", "/admin/api/requests?key=hash-alice&limit=10", nil, nil); r.status != http.StatusOK || !strings.Contains(r.raw, `"key":"alice","key_hash":"hash-alice"`) || !strings.Contains(r.raw, `"cached":40`) || !strings.Contains(r.raw, `"anchor":"ok"`) {
		t.Fatalf("requests = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/requests?key=alice&limit=10", nil, nil); r.raw != "[]\n" {
		t.Fatalf("requests filtered by name = %s, want none (the filter takes a hash)", r.raw)
	}
	if r := c.do("GET", "/admin/api/usage/filters?since=0", nil, nil); !strings.Contains(r.raw, `"key":[{"hash":"hash-alice","name":"alice"}]`) || !strings.Contains(r.raw, fmt.Sprintf(`"earliest_at":%d`, at.UnixMilli())) {
		t.Fatalf("filters = %s", r.raw)
	}
	if r := c.do("GET", "/admin/api/usage?by=nothing", nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad grouping = %d", r.status)
	}
}

func TestUserKeyUsageAndMetadata(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	adminClient := e.client("203.0.113.1")
	adminClient.login(testPassword)

	r := adminClient.do("POST", "/admin/api/keys", map[string]any{
		"name": "alice", "note": "secret-admin-note", "models": []string{"gemini-3.8-flash"},
		"max_workers": 3, "limit_total": 15.0,
	}, nil)
	aliceToken, _ := r.body["key"].(string)
	aliceSum := sha256.Sum256([]byte(aliceToken))
	aliceHash := hex.EncodeToString(aliceSum[:])

	r = adminClient.do("POST", "/admin/api/keys", map[string]any{"name": "bob", "note": "bob-note"}, nil)
	bobToken, _ := r.body["key"].(string)
	bobSum := sha256.Sum256([]byte(bobToken))
	bobHash := hex.EncodeToString(bobSum[:])

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	costA, costB := 1.25, 2.5
	for _, req := range []store.Request{
		{At: now, KeyHash: aliceHash, KeyName: "alice", Account: "main", QuotaGroup: "gemini", Model: "gemini-3.8-flash", Status: store.StatusOK, Input: 100, Cached: 40, Output: 10, CostUSD: &costA},
		{At: now, KeyHash: bobHash, KeyName: "bob", Account: "other", QuotaGroup: "gemini", Model: "claude-opus-5-5", Status: store.StatusOK, Input: 500, Output: 50, CostUSD: &costB},
	} {
		if err := e.st.AddRequest(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	reqs, err := e.st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %+v, %v", reqs, err)
	}
	// reqs are newest first: [bob, alice].
	if err := e.st.AttributeQuota(ctx, "main", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[1].ID: 0.15}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AttributeQuota(ctx, "other", "gemini-weekly", now.Add(time.Minute), map[int64]float64{reqs[0].ID: 0.35}); err != nil {
		t.Fatal(err)
	}

	user := e.client("203.0.113.9")
	if page := user.do("GET", "/user/", nil, nil); page.status != http.StatusOK || !strings.Contains(page.raw, "app.js") {
		t.Fatalf("GET /user/ = %d", page.status)
	}
	if r := user.do("GET", "/user/api/key", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("missing key = %d %s", r.status, r.raw)
	}
	if r := user.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer sk-wrong"}); r.status != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d %s", r.status, r.raw)
	}

	auth := map[string]string{"Authorization": "Bearer " + aliceToken}
	r = user.do("GET", "/user/api/key", nil, auth)
	if r.status != http.StatusOK || strings.Contains(r.raw, "secret-admin-note") {
		t.Fatalf("GET /user/api/key = %d %s", r.status, r.raw)
	}
	var kv userKeyView
	if err := json.Unmarshal([]byte(r.raw), &kv); err != nil {
		t.Fatal(err)
	}
	wantUserQuota := map[string]keyQuotaView{
		"gemini": {Used: 0.15, Accounts: []accountQuotaView{{Account: "main", Used: 0.15}}},
	}
	if kv.Key.Name != "alice" || kv.Key.Hash != aliceHash || kv.Key.Note != "" || kv.Key.MaxWorkers == nil || *kv.Key.MaxWorkers != 3 ||
		kv.Key.LimitTotal == nil || *kv.Key.LimitTotal != 15.0 || kv.Key.Spent["gemini"] != 1.25 || !reflect.DeepEqual(kv.Key.Quota, wantUserQuota) || kv.Key.Totals.Requests != 1 || kv.Key.RevokedAt != nil {
		t.Fatalf("user key view = %+v", kv)
	}

	// Attempting to pass ?key=<bobHash> on /user/api/* is ignored and stays pinned to alice.
	r = user.do("GET", "/user/api/requests?key="+bobHash+"&limit=10", nil, auth)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `"key":"alice"`) || strings.Contains(r.raw, `"bob"`) {
		t.Fatalf("user requests = %d %s", r.status, r.raw)
	}
	r = user.do("GET", "/user/api/usage?by=key&key="+bobHash, nil, auth)
	if r.status != http.StatusOK || !strings.Contains(r.raw, aliceHash) || strings.Contains(r.raw, bobHash) {
		t.Fatalf("user usage = %d %s", r.status, r.raw)
	}
	wantUserHour := now.UTC().Format("2006-01-02 15:00")
	r = user.do("GET", "/user/api/usage?by=hour_key&tz=0&key="+bobHash, nil, auth)
	if r.status != http.StatusOK || !strings.Contains(r.raw, wantUserHour+`\u001f`+aliceHash) || strings.Contains(r.raw, bobHash) {
		t.Fatalf("user usage by hour_key = %d %s", r.status, r.raw)
	}
	// An older request from bob must not leak into alice's earliest_at bound.
	if err := e.st.AddRequest(ctx, store.Request{At: now.Add(-2 * time.Hour), KeyHash: bobHash, KeyName: "bob", Account: "other", Model: "claude-opus-5-5", Status: store.StatusOK}); err != nil {
		t.Fatal(err)
	}
	r = user.do("GET", "/user/api/usage/filters?since=0", nil, auth)
	if r.status != http.StatusOK || !strings.Contains(r.raw, `"key":[]`) || !strings.Contains(r.raw, `"gemini-3.8-flash"`) || strings.Contains(r.raw, `"claude-opus-5-5"`) || strings.Contains(r.raw, `"other"`) || !strings.Contains(r.raw, fmt.Sprintf(`"earliest_at":%d`, now.UnixMilli())) {
		t.Fatalf("user filters = %d %s", r.status, r.raw)
	}

	// Revoked keys can still view their metadata and usage history on /user.
	if r := adminClient.do("POST", "/admin/api/keys/"+aliceHash+"/revoke", nil, nil); r.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", r.status, r.raw)
	}
	r = user.do("GET", "/user/api/key", nil, auth)
	if err := json.Unmarshal([]byte(r.raw), &kv); r.status != http.StatusOK || err != nil || kv.Key.RevokedAt == nil {
		t.Fatalf("revoked key on /user/api/key = %d %s", r.status, r.raw)
	}
	if r := user.do("GET", "/user/api/requests?limit=10", nil, auth); r.status != http.StatusOK || !strings.Contains(r.raw, `"key":"alice"`) {
		t.Fatalf("revoked key on /user/api/requests = %d %s", r.status, r.raw)
	}

	// Deleted keys are immediately evicted from the in-memory cache.
	if r := adminClient.do("DELETE", "/admin/api/keys/"+aliceHash, nil, nil); r.status != http.StatusOK {
		t.Fatalf("delete = %d %s", r.status, r.raw)
	}
	if r := user.do("GET", "/user/api/key", nil, auth); r.status != http.StatusUnauthorized {
		t.Fatalf("deleted key on /user/api/key = %d %s", r.status, r.raw)
	}
}

func TestUserKeyDelayAndLockout(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	adminClient := e.client("203.0.113.1")
	adminClient.login(testPassword)
	r := adminClient.do("POST", "/admin/api/keys", map[string]any{"name": "alice"}, nil)
	aliceToken, _ := r.body["key"].(string)

	attacker := e.client("198.51.100.77")
	for i := range userKeyLimits.PerClient {
		start := time.Now()
		resp := attacker.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer sk-wrong"})
		elapsed := time.Since(start)
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d %s", i, resp.status, resp.raw)
		}
		if elapsed < minUserKeyDelay {
			t.Fatalf("attempt %d elapsed = %v, want >= %v", i, elapsed, minUserKeyDelay)
		}
	}

	// Locked-out requests fail immediately with 429 without sleeping.
	start := time.Now()
	locked := attacker.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer " + aliceToken})
	if locked.status != http.StatusTooManyRequests || locked.header.Get("Retry-After") == "" {
		t.Fatalf("locked response = %d %s (Retry-After=%q)", locked.status, locked.raw, locked.header.Get("Retry-After"))
	}
	if elapsed := time.Since(start); elapsed >= minUserKeyDelay {
		t.Fatalf("locked response took %v, want immediate 429", elapsed)
	}

	// Other IPs are unaffected (no global lockout).
	other := e.client("203.0.113.20")
	start = time.Now()
	okResp := other.do("GET", "/user/api/key", nil, map[string]string{"Authorization": "Bearer " + aliceToken})
	if okResp.status != http.StatusOK {
		t.Fatalf("other client = %d %s", okResp.status, okResp.raw)
	}
	if elapsed := time.Since(start); elapsed < minUserKeyDelay {
		t.Fatalf("valid key elapsed = %v, want >= %v", elapsed, minUserKeyDelay)
	}
}

func TestRenameAccount(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	r1 := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}, nil)
	r2 := c.do("POST", "/admin/api/accounts", map[string]any{"name": "backup"}, nil)
	if r1.status != http.StatusOK || r2.status != http.StatusOK {
		t.Fatalf("add = %d / %d", r1.status, r2.status)
	}
	id1 := r1.body["id"].(string)

	// Start agy and sample quota on id1; capture its live PID so we verify
	// renaming does not restart agy or move its HOME directory.
	if r := c.do("POST", "/admin/api/accounts/"+id1+"/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("refresh = %d %s", r.status, r.raw)
	}
	procsBefore := e.p.Account(id1).LiveProcs()
	if len(procsBefore) != 1 {
		t.Fatalf("live procs = %d, want 1", len(procsBefore))
	}
	pidBefore := procsBefore[0].PID()

	// Duplicate name -> 400; missing ID -> 404; empty/control char -> 400.
	if r := c.do("PUT", "/admin/api/accounts/"+id1+"/name", map[string]string{"name": "backup"}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("duplicate rename = %d %s", r.status, r.raw)
	}
	if r := c.do("PUT", "/admin/api/accounts/missing/name", map[string]string{"name": "next"}, nil); r.status != http.StatusNotFound {
		t.Fatalf("missing rename = %d %s", r.status, r.raw)
	}
	if r := c.do("PUT", "/admin/api/accounts/"+id1+"/name", map[string]string{"name": "   "}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("empty rename = %d %s", r.status, r.raw)
	}

	// Rename to a human-readable name with spaces, @, and Unicode.
	newName := "Alice Pro (主力) <alice@example.com>"
	if r := c.do("PUT", "/admin/api/accounts/"+id1+"/name", map[string]string{"name": "  " + newName + "  "}, nil); r.status != http.StatusOK {
		t.Fatalf("rename = %d %s", r.status, r.raw)
	}

	procsAfter := e.p.Account(id1).LiveProcs()
	if len(procsAfter) != 1 || procsAfter[0].PID() != pidBefore {
		t.Fatalf("agy restarted on rename: before pid %d, after %v", pidBefore, procsAfter)
	}

	var accounts []accountView
	if err := json.Unmarshal([]byte(c.do("GET", "/admin/api/accounts", nil, nil).raw), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != id1 || accounts[0].Name != newName || accounts[0].User == nil || len(accounts[0].Models) == 0 {
		t.Fatalf("accounts after rename = %+v", accounts)
	}

	entries, err := e.st.AuditLog(context.Background(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := slices.ContainsFunc(entries, func(a store.AuditEntry) bool {
		return a.Action == "account_renamed" && a.Detail == "main → "+newName
	})
	if !foundAudit {
		t.Fatalf("audit missing account_renamed: %+v", entries)
	}
}

func TestLegacyAccountDirAndSnapshotMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agyrelay.db")
	rawDB, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE accounts (
			name       TEXT PRIMARY KEY,
			env        TEXT NOT NULL DEFAULT '[]',
			proxy      TEXT NOT NULL DEFAULT '',
			paused     INTEGER NOT NULL DEFAULT 0,
			priority   INTEGER NOT NULL DEFAULT 2,
			models     TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL DEFAULT 1000
		);
		INSERT INTO accounts (name, priority) VALUES ('main', 2);
	`); err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	// Legacy directory <dir>/accounts/main and snapshots.json referencing "main".
	legacyMarker := filepath.Join(dir, "accounts", "main", ".gemini", "marker")
	if err := os.MkdirAll(filepath.Dir(legacyMarker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyMarker, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(dir, "snapshots.json")
	snapJSON := `[{"user":"h1","account":"main","sessionId":"S1","toolsKey":"","count":2,"hash":"abc","steps":2,"updatedAt":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}]`
	if err := os.WriteFile(snapPath, []byte(snapJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	b, err := bridge.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	p, err := pool.New(pool.Config{Bin: os.Args[0], TurnsPerAccount: 1, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	settings, err := control.LoadSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	settings.AllowDirect = true
	recorder := usage.NewRecorder(st, settings.Prices, nil, log)
	budget := usage.NewBudget(st, recorder)
	flags, err := promptflags.New(context.Background(), st, log)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Pool: p, Bridge: b, DataDir: dir, SnapshotTTL: time.Hour, IdleClose: time.Hour, Budget: budget, Flags: flags, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	monitor := usage.NewMonitor(usage.MonitorConfig{Store: st, Pool: p, Profile: gateway.IdleProfile(), Log: log})
	outbound, err := egress.Start(log)
	if err != nil {
		t.Fatal(err)
	}
	defer outbound.Close()
	cloudCode, err := cloudcode.Start(cloudcode.DefaultUpstream, log)
	if err != nil {
		t.Fatal(err)
	}
	defer cloudCode.Close()

	if _, err := control.New(context.Background(), control.Config{
		Store: st, Pool: p, Gateway: gw, Monitor: monitor, Recorder: recorder, Budget: budget, Flags: flags, Egress: outbound, CloudCode: cloudCode, AgyBin: os.Args[0], Level: new(slog.LevelVar),
		AccountsDir: filepath.Join(dir, "accounts"), Settings: settings, Log: log,
	}); err != nil {
		t.Fatal(err)
	}

	accounts := p.Accounts()
	if len(accounts) != 1 || accounts[0].Name() != "main" || accounts[0].ID() == "main" || accounts[0].Priority() != 2 {
		t.Fatalf("migrated pool accounts = %+v", accounts)
	}
	migratedID := accounts[0].ID()
	if _, err := os.Stat(filepath.Join(dir, "accounts", "main")); !os.IsNotExist(err) {
		t.Fatalf("legacy accounts/main dir still exists: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "accounts", migratedID, ".gemini", "marker")); err != nil || string(raw) != "ok" {
		t.Fatalf("migrated marker = %q, %v", raw, err)
	}
	updatedSnaps, err := os.ReadFile(snapPath)
	if err != nil || !strings.Contains(string(updatedSnaps), `"account":"`+migratedID+`"`) {
		t.Fatalf("migrated snapshots.json = %s, %v", updatedSnaps, err)
	}
}

func TestWarmupSettingsAndCallsView(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := SetPassword(ctx, e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	// GET settings returns warmup defaults and stays round-trippable to PUT once defaults is removed.
	r := c.do("GET", "/admin/api/settings", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET settings = %d %s", r.status, r.raw)
	}
	if r.body["warmup_full_pools"] != false {
		t.Fatalf("warmup_full_pools = %v, want false", r.body["warmup_full_pools"])
	}
	roundTrip := maps.Clone(r.body)
	delete(roundTrip, "defaults")
	if rr := c.do("PUT", "/admin/api/settings", roundTrip, nil); rr.status != http.StatusOK {
		t.Fatalf("round-trip PUT settings = %d %s", rr.status, rr.raw)
	}

	base := map[string]any{
		"turns_per_account": 3, "max_workers": 10, "snapshot_ttl": "2h",
		"session_idle_close": "5m", "park_ttl": "10m", "log_level": "debug",
		"retention_days": 30, "capture_response_mib": 4, "flagged_prompt_retention_days": 14,
		"prices": []map[string]any{{"model": "gemini-3.8-flash", "input": 1.5, "cached": 0.15, "output": 7.5}},
	}

	// Reject mismatched pool model.
	bad := maps.Clone(base)
	bad["warmup_full_pools"] = true
	bad["warmup_models"] = map[string]string{"gemini": "claude-sonnet-5-5"}
	if r := c.do("PUT", "/admin/api/settings", bad, nil); r.status != http.StatusBadRequest {
		t.Fatalf("mismatched warmup model = %d %s, want 400", r.status, r.raw)
	}

	// Accept valid warmup settings.
	good := maps.Clone(base)
	good["warmup_full_pools"] = true
	good["warmup_models"] = map[string]string{"gemini": "gemini-3.8-flash", "claude-gpt": "claude-sonnet-5-5"}
	if r := c.do("PUT", "/admin/api/settings", good, nil); r.status != http.StatusOK {
		t.Fatalf("valid warmup settings = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings(); !got.WarmupFullPools || got.WarmupModels["gemini"] != "gemini-3.8-flash" || got.WarmupModels["claude-gpt"] != "claude-sonnet-5-5" {
		t.Fatalf("applied warmup settings = %+v", got)
	}

	// Omitting warmup_models preserves active overrides; explicit {} clears them.
	omit := maps.Clone(base)
	omit["warmup_full_pools"] = true
	if r := c.do("PUT", "/admin/api/settings", omit, nil); r.status != http.StatusOK {
		t.Fatalf("omit warmup_models = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings().WarmupModels; got["gemini"] != "gemini-3.8-flash" {
		t.Fatalf("omitted warmup_models lost overrides: %+v", got)
	}
	clear := maps.Clone(base)
	clear["warmup_full_pools"] = false
	clear["warmup_models"] = map[string]string{}
	if r := c.do("PUT", "/admin/api/settings", clear, nil); r.status != http.StatusOK {
		t.Fatalf("clear warmup_models = %d %s", r.status, r.raw)
	}
	if got := e.ctl.Settings(); got.WarmupFullPools || len(got.WarmupModels) != 0 {
		t.Fatalf("cleared warmup settings = %+v", got)
	}

	// Warmup calls show in admin Calls APIs and never leak to /user/api/requests.
	keyToken, _, err := e.ctl.CreateKey(ctx, store.KeyUpdate{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.0005
	if err := e.st.AddRequest(ctx, store.Request{
		At: time.Now(), Duration: time.Second, KeyHash: store.SystemKeyWarmup, KeyName: "warmup",
		Account: "acct1", AccountName: "main", Model: "gemini-3.8-flash", Slug: "gemini-3.8-flash-low", Effort: "low",
		Status: store.StatusOK, Finish: "stop",
		Input: 50, Output: 2, CostUSD: &cost, QuotaGroup: "gemini",
	}); err != nil {
		t.Fatal(err)
	}

	reqs := c.do("GET", "/admin/api/requests?key="+store.SystemKeyWarmup, nil, nil)
	if reqs.status != http.StatusOK {
		t.Fatalf("admin requests = %d %s", reqs.status, reqs.raw)
	}
	var adminRows []map[string]any
	if err := json.Unmarshal([]byte(reqs.raw), &adminRows); err != nil || len(adminRows) != 1 || adminRows[0]["api"] != "" || adminRows[0]["mode"] != "" || adminRows[0]["key"] != "warmup" {
		t.Fatalf("admin warmup requests = %s, %v", reqs.raw, err)
	}

	user := e.client("203.0.113.2")
	userReply := user.do("GET", "/user/api/requests?key="+store.SystemKeyWarmup, nil, map[string]string{"Authorization": "Bearer " + keyToken})
	if userReply.status != http.StatusOK {
		t.Fatalf("user requests = %d %s", userReply.status, userReply.raw)
	}
	var userRows []map[string]any
	if err := json.Unmarshal([]byte(userReply.raw), &userRows); err != nil || len(userRows) != 0 {
		t.Fatalf("user requests leaked warmup calls: %s, %v", userReply.raw, err)
	}
}

func TestAccountTest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := SetPassword(ctx, e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	add := c.do("POST", "/admin/api/accounts", map[string]any{"name": "main", "env": []string{agytest.EnvRun + "=1"}}, nil)
	if add.status != http.StatusOK {
		t.Fatalf("add = %d %s", add.status, add.raw)
	}
	path := "/admin/api/accounts/" + add.body["id"].(string) + "/test"

	// A refresh starts agy, and its probe session reports the models.
	if r := c.do("POST", "/admin/api/accounts/"+add.body["id"].(string)+"/refresh", nil, nil); r.status != http.StatusOK {
		t.Fatalf("refresh = %d %s", r.status, r.raw)
	}

	r := c.do("POST", path, map[string]any{"model": "claude-opus-5-5"}, nil)
	if r.status != http.StatusOK || r.body["reply"] != "reply to: who are you" || r.body["model"] != "claude-opus-5-5" || r.body["slug"] != "claude-opus-5-5-high" {
		t.Fatalf("test with a model = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", path, map[string]any{"model": ""}, nil); r.status != http.StatusOK || r.body["reply"] != "reply to: who are you" {
		t.Fatalf("test with the cheapest model = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", path, map[string]any{"model": "no-such-model"}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("test with an unoffered model = %d %s, want 400", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/accounts/missing/test", map[string]any{"model": ""}, nil); r.status != http.StatusNotFound {
		t.Fatalf("test of an unknown account = %d %s, want 404", r.status, r.raw)
	}

	rows := c.do("GET", "/admin/api/requests?key="+store.SystemKeyTest, nil, nil)
	var calls []map[string]any
	if err := json.Unmarshal([]byte(rows.raw), &calls); err != nil || len(calls) != 2 || calls[0]["key"] != "test" || calls[0]["api"] != "" {
		t.Fatalf("admin test requests = %s, %v", rows.raw, err)
	}
}
