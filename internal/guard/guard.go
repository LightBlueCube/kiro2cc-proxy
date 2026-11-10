// Package guard identifies clients and limits failed authentication
// attempts.
package guard

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// ClientIP is the address a request came from. Behind a Cloudflare Tunnel
// cloudflared connects from loopback and CF-Connecting-IP carries the
// visitor's address; the header is trusted only on loopback connections,
// so a direct client cannot set it.
func ClientIP(r *http.Request) netip.Addr {
	peer := peerAddr(r)
	if peer.IsLoopback() {
		if forwarded, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP")); err == nil {
			return forwarded.Unmap()
		}
	}
	return peer
}

// ViaHTTPS reports whether the client reached us over HTTPS: through a
// loopback proxy that says so, or directly over TLS.
func ViaHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return peerAddr(r).IsLoopback() && r.Header.Get("X-Forwarded-Proto") == "https"
}

func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return peer.Unmap()
}

// Limiter counts failures per client and in total, and refuses clients
// over their budget. IPv6 clients are grouped by /64, since one host
// usually owns a whole /64.
//
//	per client: PerClient failures within Window lock the client out for
//	            Lockout, doubling with each lockout up to MaxLockout
//	in total:   Global failures within Window make everyone wait
//
// The total limit bounds guessing even when client addresses are forged.
type Limiter struct {
	cfg Config
	now func() time.Time

	mu        sync.Mutex
	clients   map[netip.Prefix]*clientFailures
	recent    []time.Time
	lastPurge time.Time
}

type Config struct {
	PerClient  int
	Global     int
	Window     time.Duration
	Lockout    time.Duration
	MaxLockout time.Duration
}

type clientFailures struct {
	times   []time.Time
	until   time.Time
	strikes int
}

func NewLimiter(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, now: time.Now, clients: map[netip.Prefix]*clientFailures{}}
}

func clientKey(addr netip.Addr) netip.Prefix {
	bits := 32
	if addr.Is6() {
		bits = 64
	}
	prefix, _ := addr.Prefix(bits)
	return prefix
}

// Allow reports whether addr may try now, and if not, how long it waits.
func (l *Limiter) Allow(addr netip.Addr) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.purgeLocked(now)
	if c := l.clients[clientKey(addr)]; c != nil && now.Before(c.until) {
		return false, c.until.Sub(now)
	}
	if len(l.recent) >= l.cfg.Global {
		return false, l.recent[0].Add(l.cfg.Window).Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt from addr.
func (l *Limiter) Fail(addr netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.recent = append(l.recent, now)
	key := clientKey(addr)
	c := l.clients[key]
	if c == nil {
		c = &clientFailures{}
		l.clients[key] = c
	}
	c.times = append(trimBefore(c.times, now.Add(-l.cfg.Window)), now)
	if len(c.times) < l.cfg.PerClient {
		return
	}
	lockout := l.cfg.Lockout << min(c.strikes, 16)
	c.until = now.Add(min(lockout, l.cfg.MaxLockout))
	c.strikes++
	c.times = nil
}

// Succeed forgets addr's failures.
func (l *Limiter) Succeed(addr netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, clientKey(addr))
}

// purgeLocked drops expired state, sweeping clients at most once per
// window. Allow refuses once Global failures are recent, so at most Global
// clients are added per window.
func (l *Limiter) purgeLocked(now time.Time) {
	l.recent = trimBefore(l.recent, now.Add(-l.cfg.Window))
	if now.Sub(l.lastPurge) < l.cfg.Window {
		return
	}
	l.lastPurge = now
	for key, c := range l.clients {
		c.times = trimBefore(c.times, now.Add(-l.cfg.Window))
		forgiven := now.After(c.until.Add(l.cfg.MaxLockout))
		if len(c.times) == 0 && forgiven {
			delete(l.clients, key)
		}
	}
}

func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}
