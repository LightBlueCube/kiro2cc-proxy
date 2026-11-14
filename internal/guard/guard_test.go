package guard

import (
	"crypto/tls"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
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

func TestLimiterNoGlobalLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(Config{PerClient: 3, Global: 0, Window: 10 * time.Minute, Lockout: 10 * time.Second, MaxLockout: time.Hour})
	l.now = func() time.Time { return now }

	for i := range 50 {
		addr := netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)})
		for range 3 {
			l.Fail(addr)
		}
	}
	bystander := netip.MustParseAddr("203.0.113.50")
	if ok, wait := l.Allow(bystander); !ok || wait != 0 {
		t.Fatalf("bystander blocked without global limit: ok=%v wait=%v", ok, wait)
	}
}

func TestLimiterLockoutPerError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(Config{
		PerClient:       3,
		Window:          15 * time.Minute,
		Lockout:         15 * time.Second,
		MaxLockout:      2 * time.Minute,
		LockoutPerError: true,
	})
	l.now = func() time.Time { return now }

	attacker := netip.MustParseAddr("198.51.100.7")
	// First 2 failures do not lock out yet.
	for i := range 2 {
		l.Fail(attacker)
		if ok, _ := l.Allow(attacker); !ok {
			t.Fatalf("locked out too early after failure %d", i+1)
		}
	}

	// 3rd failure -> 15s; every subsequent failure doubles up to 2m.
	wantWaits := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute}
	for i, want := range wantWaits {
		l.Fail(attacker)
		if ok, wait := l.Allow(attacker); ok || wait != want {
			t.Fatalf("step %d: Allow = (%v, %v), want (false, %v)", i, ok, wait, want)
		}
		now = now.Add(want)
		if ok, wait := l.Allow(attacker); !ok || wait != 0 {
			t.Fatalf("step %d after %v: Allow = (%v, %v), want (true, 0)", i, want, ok, wait)
		}
	}

	// Staying quiet past MaxLockout forgives the client.
	now = now.Add(2*time.Minute + time.Second)
	l.Fail(attacker)
	if ok, _ := l.Allow(attacker); !ok {
		t.Fatal("forgiven client locked out on 1st new failure")
	}
}
