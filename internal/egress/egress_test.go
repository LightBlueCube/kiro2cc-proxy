package egress

import (
	"bufio"
	"encoding/base64"
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
