// Package egress is the loopback HTTP proxy every agy process uses for
// its outbound traffic. Each account authenticates with its own token;
// its connections go through the account's route: an upstream HTTP proxy,
// a direct connection (only when the admin allows it), or nowhere. A
// missing proxy setting therefore refuses connections instead of quietly
// connecting directly.
//
//	agy ──CONNECT host:443, Basic account:token──▶ egress ──▶ upstream proxy ──▶ host
//	                                                     └──▶ host            (direct, if allowed)
//	                                                     └──▶ 403             (no route)
//
// agy 1.5.3 sends all its traffic through HTTPS_PROXY with CONNECT and
// passes the URL's user info as Proxy-Authorization (verified).
package egress

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	dialTimeout = 30 * time.Second
	// headTimeout bounds reading a CONNECT request or an upstream reply.
	headTimeout = 30 * time.Second
	// recentHosts is how many distinct hosts an account's stats keep.
	recentHosts = 20
)

// Route is where an account's connections go. The zero Route refuses
// them.
type Route struct {
	// Proxy is an upstream http:// proxy URL, possibly with user info.
	Proxy string
	// Direct connects to the target itself; it is set only when the
	// admin allows direct connections.
	Direct bool
}

// Refused reports whether the route lets no connection out.
func (r Route) Refused() bool { return r.Proxy == "" && !r.Direct }

// Server is the loopback proxy shared by all accounts.
type Server struct {
	log      *slog.Logger
	listener net.Listener

	mu       sync.Mutex
	accounts map[string]*account
}

type account struct {
	name  string
	token string
	route Route
	// tunnels are the open connections; Reroute closes them.
	tunnels map[*tunnel]struct{}
	stats   Stats
}

type tunnel struct {
	client, upstream net.Conn
}

func (t *tunnel) close() {
	t.client.Close()
	t.upstream.Close()
}

// Stats describe an account's traffic since the relay started.
type Stats struct {
	// Route describes the current route without credentials: "proxy
	// http://host:port", "direct", or "refused".
	Route       string
	Open        int
	Connections int64
	BytesOut    int64
	BytesIn     int64
	Refused     int64
	// LastError is the newest refused or failed connection.
	LastError   string
	LastErrorAt time.Time
	// Hosts are the most recently used targets, newest first.
	Hosts []HostStat
}

type HostStat struct {
	Host        string
	Connections int64
	LastAt      time.Time
}

// Start listens on a loopback port.
func Start(log *slog.Logger) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	s := &Server{log: log, listener: listener, accounts: map[string]*account{}}
	go s.serve()
	return s, nil
}

// Close stops accepting; open tunnels end with their processes.
func (s *Server) Close() error { return s.listener.Close() }

// Register adds the account (or keeps its token) and returns the proxy
// URL its agy must use for HTTPS_PROXY.
func (s *Server) Register(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[name]
	if a == nil {
		a = &account{name: name, token: rand.Text(), tunnels: map[*tunnel]struct{}{}}
		a.stats.Route = describe(a.route)
		s.accounts[name] = a
	}
	return (&url.URL{Scheme: "http", User: url.UserPassword(name, a.token), Host: s.listener.Addr().String()}).String()
}

// Forget removes the account and closes its tunnels.
func (s *Server) Forget(name string) {
	s.mu.Lock()
	a := s.accounts[name]
	delete(s.accounts, name)
	tunnels := a.takeTunnels()
	s.mu.Unlock()
	closeAll(tunnels)
}

// SetRoute changes where the account's connections go. When the route
// changes, open tunnels are closed so agy reconnects through the new one.
func (s *Server) SetRoute(name string, route Route) {
	s.mu.Lock()
	a := s.accounts[name]
	if a == nil || a.route == route {
		s.mu.Unlock()
		return
	}
	a.route = route
	a.stats.Route = describe(route)
	tunnels := a.takeTunnels()
	s.mu.Unlock()
	closeAll(tunnels)
	s.log.Info("egress route changed", "account", name, "route", describe(route), "closed", len(tunnels))
}

// Stats returns the account's traffic, or false when it is unknown.
func (s *Server) Stats(name string) (Stats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[name]
	if a == nil {
		return Stats{}, false
	}
	st := a.stats
	st.Open = len(a.tunnels)
	st.Hosts = slices.Clone(st.Hosts)
	return st, true
}

func (a *account) takeTunnels() []*tunnel {
	if a == nil {
		return nil
	}
	out := make([]*tunnel, 0, len(a.tunnels))
	for t := range a.tunnels {
		out = append(out, t)
	}
	clear(a.tunnels)
	return out
}

func closeAll(tunnels []*tunnel) {
	for _, t := range tunnels {
		t.close()
	}
}

func describe(r Route) string {
	switch {
	case r.Proxy != "":
		u, err := url.Parse(r.Proxy)
		if err != nil {
			return "proxy (unreadable)"
		}
		return "proxy " + (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	case r.Direct:
		return "direct"
	}
	return "refused"
}

func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Error("egress proxy stopped", "err", err)
			}
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(client net.Conn) {
	client.SetDeadline(time.Now().Add(headTimeout))
	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		client.Close()
		return
	}
	a, route, ok := s.authenticate(req)
	if !ok {
		s.log.Warn("egress connection with a bad token", "target", req.Host)
		reply(client, http.StatusProxyAuthRequired, "Proxy-Authenticate: Basic realm=\"agyrelay\"\r\n")
		return
	}
	if req.Method != http.MethodConnect {
		s.fail(a, req.Host, fmt.Sprintf("%s %s refused: only CONNECT is proxied", req.Method, req.URL))
		reply(client, http.StatusMethodNotAllowed, "")
		return
	}
	target := req.Host
	if route.Refused() {
		s.fail(a, target, "no proxy configured for the account, no fallback proxy, and direct connections are not allowed")
		reply(client, http.StatusForbidden, "")
		return
	}

	upstream, err := dialRoute(route, target)
	if err != nil {
		s.fail(a, target, err.Error())
		reply(client, http.StatusBadGateway, "")
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	client.SetDeadline(time.Time{})

	t := &tunnel{client: client, upstream: upstream}
	if !s.open(a.name, route, t, target) {
		t.close() // the route changed while dialing; agy reconnects
		return
	}
	var out int64
	done := make(chan struct{})
	go func() {
		out, _ = io.Copy(upstream, reader) // reader holds bytes sent after the CONNECT head
		t.close()
		close(done)
	}()
	in, _ := io.Copy(client, upstream)
	t.close()
	<-done
	s.closed(a.name, t, out, in)
}

// authenticate maps Proxy-Authorization to the account and its route.
func (s *Server) authenticate(req *http.Request) (*account, Route, bool) {
	name, token, ok := basicAuth(req.Header.Get("Proxy-Authorization"))
	if !ok {
		return nil, Route{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[name]
	if a == nil || subtle.ConstantTimeCompare([]byte(a.token), []byte(token)) != 1 {
		return nil, Route{}, false
	}
	return a, a.route, true
}

func basicAuth(header string) (user, password string, ok bool) {
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(raw), ":")
}

// open records a tunnel dialed over route; it reports false when the
// account is gone or its route changed meanwhile.
func (s *Server) open(name string, route Route, t *tunnel, target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[name]
	if a == nil || a.route != route {
		return false
	}
	a.tunnels[t] = struct{}{}
	a.stats.Connections++
	host, _, _ := net.SplitHostPort(target)
	a.stats.Hosts = touchHost(a.stats.Hosts, host, time.Now())
	return true
}

func (s *Server) closed(name string, t *tunnel, out, in int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[name]
	if a == nil {
		return
	}
	delete(a.tunnels, t)
	a.stats.BytesOut += out
	a.stats.BytesIn += in
}

func (s *Server) fail(a *account, target, reason string) {
	s.log.Error("egress connection refused", "account", a.name, "target", target, "reason", reason)
	s.mu.Lock()
	defer s.mu.Unlock()
	a.stats.Refused++
	a.stats.LastError, a.stats.LastErrorAt = target+": "+reason, time.Now()
}

// touchHost moves host to the front of the recent list.
func touchHost(hosts []HostStat, host string, at time.Time) []HostStat {
	entry := HostStat{Host: host, LastAt: at, Connections: 1}
	if i := slices.IndexFunc(hosts, func(h HostStat) bool { return h.Host == host }); i >= 0 {
		entry.Connections += hosts[i].Connections
		hosts = slices.Delete(hosts, i, i+1)
	}
	hosts = slices.Insert(hosts, 0, entry)
	if len(hosts) > recentHosts {
		hosts = hosts[:recentHosts]
	}
	return hosts
}

func reply(conn net.Conn, status int, headers string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status), headers)
	conn.Close()
}

// dialRoute connects to target directly or through the upstream proxy.
func dialRoute(route Route, target string) (net.Conn, error) {
	if route.Direct {
		conn, err := net.DialTimeout("tcp", target, dialTimeout)
		if err != nil {
			return nil, fmt.Errorf("direct connection failed: %w", err)
		}
		return conn, nil
	}
	proxy, err := url.Parse(route.Proxy)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy URL: %w", err)
	}
	conn, err := net.DialTimeout("tcp", proxy.Host, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy %s unreachable: %w", proxy.Host, err)
	}
	if err := connectThrough(conn, proxy, target); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// connectThrough opens a CONNECT tunnel to target over conn.
func connectThrough(conn net.Conn, proxy *url.URL, target string) error {
	conn.SetDeadline(time.Now().Add(headTimeout))
	defer conn.SetDeadline(time.Time{})
	head := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		head += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)) + "\r\n"
	}
	if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
		return fmt.Errorf("upstream proxy %s: %w", proxy.Host, err)
	}
	// Read the reply byte by byte up to its blank line: a buffered reader
	// could swallow the first bytes of the tunnel.
	status, err := readHead(conn)
	if err != nil {
		return fmt.Errorf("upstream proxy %s: %w", proxy.Host, err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") && !strings.HasPrefix(status, "HTTP/1.0 200") {
		return fmt.Errorf("upstream proxy %s refused CONNECT %s: %s", proxy.Host, target, status)
	}
	return nil
}

// readHead reads an HTTP response head and returns its status line.
func readHead(conn net.Conn) (string, error) {
	var head []byte
	one := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		if len(head) > 16<<10 {
			return "", errors.New("reply head too long")
		}
		if _, err := conn.Read(one); err != nil {
			return "", err
		}
		head = append(head, one[0])
	}
	status, _, _ := strings.Cut(string(head), "\r\n")
	return status, nil
}
