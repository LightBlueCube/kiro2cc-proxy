package egress

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoServer answers every line with "echo: " + line.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					fmt.Fprintf(conn, "echo: %s\n", scanner.Text())
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// upstreamProxy is a CONNECT proxy that requires user:pass and records
// the targets it was asked for.
type upstreamProxy struct {
	addr string
	mu   sync.Mutex
	seen []string
}

func startUpstream(t *testing.T) *upstreamProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	u := &upstreamProxy{addr: ln.Addr().String()}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				req, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				if req.Header.Get("Proxy-Authorization") != want {
					io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				u.mu.Lock()
				u.seen = append(u.seen, req.Host)
				u.mu.Unlock()
				target, err := net.Dial("tcp", req.Host)
				if err != nil {
					io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer target.Close()
				io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
				go io.Copy(target, reader)
				io.Copy(conn, target)
			}()
		}
	}()
	return u
}

func (u *upstreamProxy) targets() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// dialVia opens a CONNECT tunnel to target through the egress proxy URL
// and returns the connection, or the refusal status.
func dialVia(t *testing.T, proxyURL, target string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, nil, resp.StatusCode
	}
	return conn, reader, http.StatusOK
}

func roundTrip(t *testing.T, conn net.Conn, reader *bufio.Reader, line string) string {
	t.Helper()
	fmt.Fprintln(conn, line)
	got, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(got)
}

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := Start(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRoutes(t *testing.T) {
	s := newServer(t)
	target := echoServer(t)
	upstream := startUpstream(t)
	proxyURL := s.Register("main")

	// No route: refused, never direct.
	if _, _, status := dialVia(t, proxyURL, target); status != http.StatusForbidden {
		t.Fatalf("no route = %d", status)
	}
	if st, _ := s.Stats("main"); st.Refused != 1 || st.Route != "refused" || !strings.Contains(st.LastError, "direct connections are not allowed") {
		t.Fatalf("stats = %+v", st)
	}

	// Upstream proxy with credentials.
	s.SetRoute("main", Route{Proxy: "http://user:pass@" + upstream.addr})
	conn, reader, status := dialVia(t, proxyURL, target)
	if status != http.StatusOK {
		t.Fatalf("via upstream = %d", status)
	}
	if got := roundTrip(t, conn, reader, "hi"); got != "echo: hi" {
		t.Fatalf("tunnel = %q", got)
	}
	if got := upstream.targets(); len(got) != 1 || got[0] != target {
		t.Fatalf("upstream saw %v", got)
	}
	st, _ := s.Stats("main")
	if st.Open != 1 || st.Connections != 1 || st.Route != "proxy http://"+upstream.addr || len(st.Hosts) != 1 || st.Hosts[0].Host != "127.0.0.1" {
		t.Fatalf("stats = %+v", st)
	}

	// A route change closes the open tunnel.
	s.SetRoute("main", Route{Direct: true})
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("tunnel survived the route change")
	}
	conn.Close()
	conn, reader, status = dialVia(t, proxyURL, target)
	if status != http.StatusOK || roundTrip(t, conn, reader, "direct") != "echo: direct" {
		t.Fatalf("direct = %d", status)
	}
	conn.Close()
	if got := upstream.targets(); len(got) != 1 {
		t.Fatalf("direct went through the upstream: %v", got)
	}

	// Upstream refusing CONNECT is a 502.
	s.SetRoute("main", Route{Proxy: "http://user:wrong@" + upstream.addr})
	if _, _, status := dialVia(t, proxyURL, target); status != http.StatusBadGateway {
		t.Fatalf("upstream 407 = %d", status)
	}
}

func TestAuthAndMethods(t *testing.T) {
	s := newServer(t)
	target := echoServer(t)
	good := s.Register("main")
	s.SetRoute("main", Route{Direct: true})
	if again := s.Register("main"); again != good {
		t.Fatalf("Register changed the token: %s vs %s", again, good)
	}

	u, _ := url.Parse(good)
	bad := (&url.URL{Scheme: "http", User: url.UserPassword("main", "guess"), Host: u.Host}).String()
	if _, _, status := dialVia(t, bad, target); status != http.StatusProxyAuthRequired {
		t.Fatalf("bad token = %d", status)
	}

	// Plain proxied HTTP is refused.
	password, _ := u.User.Password()
	req, _ := http.NewRequest(http.MethodGet, "http://"+target+"/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("main:"+password)))
	client := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return &url.URL{Scheme: "http", Host: u.Host}, nil }}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET through the proxy = %d", resp.StatusCode)
	}

	s.Forget("main")
	if _, _, status := dialVia(t, good, target); status != http.StatusProxyAuthRequired {
		t.Fatalf("forgotten account = %d", status)
	}
}

func TestParseAndMatchRules(t *testing.T) {
	rules, err := ParseRules([]string{
		"",
		"# comment line",
		"// another comment",
		"  - DOMAIN,daily-cloudcode-pa.googleapis.com,PROXY # allow model calls",
		"- 'DOMAIN,play.googleapis.com,REJECT'",
		`  - "DOMAIN-SUFFIX,.telemetry.example.com,REJECT-DROP" # leading dot`,
		"DOMAIN-KEYWORD,clearcut,REJECT",
		"DOMAIN-WILDCARD,*.metrics.?.example.org,REJECT",
		"DOMAIN-REGEX,^log[0-9]{1,2}\\.example\\.net$,REJECT",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR6,2001:db8::/32,REJECT",
		"DST-PORT,8080/9000-9010,PASS",
		"DST-PORT,9005,REJECT",
		"DST-PORT,8080,DIRECT",
		"MATCH,http://127.0.0.1:7890",
	})
	if err != nil {
		t.Fatalf("ParseRules: %v", err)
	}

	acctRoute := Route{Proxy: "http://127.0.0.1:1080"}
	cases := []struct {
		target string
		reject bool
		drop   bool
		route  Route
	}{
		{target: "daily-cloudcode-pa.googleapis.com:443", route: acctRoute},
		{target: "PLAY.googleapis.com.:443", reject: true},
		{target: "telemetry.example.com:443", reject: true, drop: true},
		{target: "sub.telemetry.example.com:443", reject: true, drop: true},
		{target: "nottelemetry.example.com:443", route: Route{Proxy: "http://127.0.0.1:7890"}},
		{target: "foo-clearcut-bar.com:443", reject: true},
		{target: "a.b.metrics.x.example.org:443", reject: true},
		{target: "a.b.metrics.xy.example.org:443", route: Route{Proxy: "http://127.0.0.1:7890"}},
		{target: "a.b.metricsX1.example.org:443", route: Route{Proxy: "http://127.0.0.1:7890"}},
		{target: "log42.example.net:443", reject: true},
		{target: "log999.example.net:443", route: Route{Proxy: "http://127.0.0.1:7890"}},
		{target: "10.1.2.3:443", route: Route{Direct: true}},
		{target: "[2001:db8::1]:443", reject: true},
		{target: "other.example.com:9005", reject: true}, // PASS on 8080/9000-9010 continued to 9005 REJECT
		{target: "other.example.com:8080", route: Route{Direct: true}},
		{target: "other.example.com:443", route: Route{Proxy: "http://127.0.0.1:7890"}},
	}
	for _, tc := range cases {
		got := resolve(rules, acctRoute, tc.target)
		if got.reject != tc.reject || got.drop != tc.drop || got.route != tc.route {
			t.Errorf("resolve(%q) = %+v, want reject=%v drop=%v route=%+v", tc.target, got, tc.reject, tc.drop, tc.route)
		}
	}

	// Wildcard '*' on DOMAIN-WILDCARD must not match IP literals.
	domWildcard, err := ParseRules([]string{"DOMAIN-WILDCARD,*,REJECT"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolve(domWildcard, acctRoute, "127.0.0.1:443"); got.reject {
		t.Fatalf("DOMAIN-WILDCARD matched IP literal: %+v", got)
	}

	invalid := []string{
		" play.googleapis.com",
		"UNKNOWN,foo.com,REJECT",
		"DOMAIN,,REJECT",
		"DOMAIN,1.2.3.4,REJECT",
		"DOMAIN-SUFFIX,1.2.3.4,REJECT",
		"DOMAIN,https://foo.com,REJECT",
		"DOMAIN,foo.com,UNKNOWN",
		"DOMAIN,foo.com,REJECT,extra",
		"DOMAIN-REGEX,[invalid,REJECT",
		"IP-CIDR,999.0.0.1/24,REJECT",
		"IP-CIDR,10.0.0.0/8,REJECT,src",
		"DST-PORT,0,REJECT",
		"DST-PORT,9000-8000,REJECT",
		"MATCH,PROXY,extra",
		"DOMAIN,foo.com,https://127.0.0.1:7890",
		"DOMAIN,foo.com,http://127.0.0.1",
	}
	for _, bad := range invalid {
		if _, err := ParseRules([]string{bad}); err == nil {
			t.Errorf("ParseRules(%q) succeeded, want error", bad)
		}
	}
}

func TestEgressRulesLiveAndTunnelReeval(t *testing.T) {
	s := newServer(t)
	echoKeep := echoServer(t)
	echoKill := echoServer(t)
	_, killPort, _ := net.SplitHostPort(echoKill)
	upstream1 := startUpstream(t)
	upstream2 := startUpstream(t)

	proxyURL := s.Register("main")
	s.SetRoute("main", Route{Proxy: "http://user:pass@" + upstream1.addr})

	defaults, err := ParseRules(DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRules(defaults)

	// Default rule rejects play.googleapis.com with 403 without polluting Refused or LastError.
	if _, _, status := dialVia(t, proxyURL, "play.googleapis.com:443"); status != http.StatusForbidden {
		t.Fatalf("default reject status = %d", status)
	}
	st, _ := s.Stats("main")
	if st.Rejected != 1 || st.Refused != 0 || st.LastError != "" {
		t.Fatalf("stats after reject = %+v", st)
	}
	if len(st.Hosts) != 1 || st.Hosts[0].Host != "play.googleapis.com" || st.Hosts[0].Rejected != 1 || st.Hosts[0].Connections != 0 {
		t.Fatalf("hosts after reject = %+v", st.Hosts)
	}

	// Open two live tunnels: echoKeep and echoKill.
	keepConn, keepReader, status := dialVia(t, proxyURL, echoKeep)
	if status != http.StatusOK {
		t.Fatalf("dial keep = %d", status)
	}
	defer keepConn.Close()

	killConn, killReader, status := dialVia(t, proxyURL, echoKill)
	if status != http.StatusOK {
		t.Fatalf("dial kill = %d", status)
	}
	defer killConn.Close()

	rules, err := ParseRules([]string{
		"DST-PORT," + killPort + ",REJECT",
		"DOMAIN,drop.example.com,REJECT-DROP",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetRules(rules)

	// The newly rejected echoKill tunnel closes immediately, while echoKeep stays alive.
	killConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := killReader.ReadString('\n'); err == nil {
		t.Fatal("newly rejected tunnel survived SetRules")
	}
	if got := roundTrip(t, keepConn, keepReader, "still-open"); got != "echo: still-open" {
		t.Fatalf("unaffected tunnel broke: %q", got)
	}

	// REJECT-DROP closes TCP without an HTTP response.
	u, _ := url.Parse(proxyURL)
	rawConn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer rawConn.Close()
	pw, _ := u.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pw))
	fmt.Fprintf(rawConn, "CONNECT drop.example.com:443 HTTP/1.1\r\nHost: drop.example.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	_, dropErr := http.ReadResponse(bufio.NewReader(rawConn), &http.Request{Method: http.MethodConnect})
	if !errors.Is(dropErr, io.EOF) && !errors.Is(dropErr, io.ErrUnexpectedEOF) {
		t.Fatalf("REJECT-DROP err = %v, want EOF", dropErr)
	}

	// Rerouting echoKeep to upstream2 closes keepConn and sends new connections via upstream2.
	rerouteRules, err := ParseRules([]string{"MATCH,http://user:pass@" + upstream2.addr})
	if err != nil {
		t.Fatal(err)
	}
	s.SetRules(rerouteRules)
	keepConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := keepReader.ReadString('\n'); err == nil {
		t.Fatal("rerouted tunnel survived SetRules")
	}
	via2Conn, via2Reader, status := dialVia(t, proxyURL, echoKeep)
	if status != http.StatusOK || roundTrip(t, via2Conn, via2Reader, "via2") != "echo: via2" {
		t.Fatalf("via upstream2 = %d", status)
	}
	via2Conn.Close()
	if got := upstream2.targets(); len(got) != 1 || got[0] != echoKeep {
		t.Fatalf("upstream2 saw %v", got)
	}
}
