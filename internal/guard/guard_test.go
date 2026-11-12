package guard

import (
	"crypto/tls"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	bridge := Proxies{netip.MustParsePrefix("172.16.0.0/12")}
	cases := []struct {
		name    string
		proxies Proxies
		peer    string
		headers map[string][]string
		want    string
	}{
		{"direct client", bridge, "203.0.113.5:4000", nil, "203.0.113.5"},
		{"direct client forging headers", bridge, "203.0.113.5:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "Cf-Connecting-Ip": {"198.51.100.2"}}, "203.0.113.5"},
		{"proxy, last hop wins", bridge, "172.17.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1, 203.0.113.9"}}, "203.0.113.9"},
		{"proxy, repeated headers", bridge, "172.17.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1", "203.0.113.9"}}, "203.0.113.9"},
		{"proxy, IPv6 hop", bridge, "172.17.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"2001:db8::7"}}, "2001:db8::7"},
		{"proxy, forwarded-for before CF-Connecting-IP", bridge, "172.17.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.9"}, "Cf-Connecting-Ip": {"198.51.100.2"}}, "203.0.113.9"},
		{"cloudflared on loopback", LoopbackProxies, "127.0.0.1:4000",
			map[string][]string{"Cf-Connecting-Ip": {"198.51.100.2"}}, "198.51.100.2"},
		{"proxy, unreadable last hop", bridge, "172.17.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.9, unknown"}, "Cf-Connecting-Ip": {"198.51.100.2"}}, "172.17.0.1"},
		{"proxy without headers", bridge, "172.17.0.1:4000", nil, "172.17.0.1"},
		{"loopback not trusted unless listed", bridge, "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.9"}}, "127.0.0.1"},
		{"IPv4-mapped peer", bridge, "[::ffff:172.17.0.1]:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.9"}}, "203.0.113.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.peer
			for k, vs := range c.headers {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if got := c.proxies.ClientIP(r).String(); got != c.want {
				t.Errorf("ClientIP = %s, want %s", got, c.want)
			}
		})
	}
}

func TestViaHTTPS(t *testing.T) {
	bridge := Proxies{netip.MustParsePrefix("172.16.0.0/12")}
	cases := []struct {
		peer, proto string
		tls         bool
		want        bool
	}{
		{"172.17.0.1:4000", "https", false, true},
		{"172.17.0.1:4000", "http", false, false},
		{"203.0.113.5:4000", "https", false, false},
		{"203.0.113.5:4000", "", true, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := bridge.ViaHTTPS(r); got != c.want {
			t.Errorf("peer %s proto %q tls %v: ViaHTTPS = %v, want %v", c.peer, c.proto, c.tls, got, c.want)
		}
	}
}
