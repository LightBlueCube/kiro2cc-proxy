// Package guard identifies clients and limits failed authentication
// attempts.
package guard

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Proxies are the peer networks trusted to report the client: a reverse
// proxy or tunnel connecting from them names the client in its forwarding
// headers. Anyone else is the client, whatever headers it sends.
type Proxies []netip.Prefix

// LoopbackProxies trusts processes on the same host only.
var LoopbackProxies = Proxies{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

func (p Proxies) trusts(peer netip.Addr) bool {
	for _, prefix := range p {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

// ClientIP is the address a request came from. From a trusted proxy it is
// the last X-Forwarded-For hop (the one the proxy added; earlier ones come
// from the client), else CF-Connecting-IP (cloudflared), else the proxy.
// An unreadable last hop leaves the proxy: CF-Connecting-IP could then be
// the client's own.
func (p Proxies) ClientIP(r *http.Request) netip.Addr {
	peer := peerAddr(r)
	if !p.trusts(peer) {
		return peer
	}
	if hops := r.Header.Values("X-Forwarded-For"); len(hops) > 0 {
		if hop, err := lastHop(hops); err == nil {
			return hop
		}
		return peer
	}
	if forwarded, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP")); err == nil {
		return forwarded.Unmap()
	}
	return peer
}

// lastHop is the last entry of X-Forwarded-For, given as repeated headers.
func lastHop(headers []string) (netip.Addr, error) {
	last := headers[len(headers)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(last))
	return addr.Unmap(), err
}

// ViaHTTPS reports whether the client reached us over HTTPS: through a
// trusted proxy that says so, or directly over TLS.
func (p Proxies) ViaHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return p.trusts(peerAddr(r)) && r.Header.Get("X-Forwarded-Proto") == "https"
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

// Limiter counts failures per client (and optionally in total), and
// refuses clients over their budget. IPv6 clients are grouped by /64,
// since one host usually owns a whole /64.
//
//	per client: PerClient failures within Window lock the client out for
//	            Lockout, doubling with each lockout up to MaxLockout
//	in total:   when Global > 0, Global failures within Window make
//	            everyone wait
type Limiter struct {
	cfg Config
	now func() time.Time

	mu        sync.Mutex
	clients   map[netip.Prefix]*clientFailures
	recent    []time.Time
	lastPurge time.Time
}

const maxTrackedClients = 100_000

type Config struct {
	PerClient  int
	Global     int
	Window     time.Duration
	Lockout    time.Duration
	MaxLockout time.Duration
	// LockoutPerError extends the client's cooldown on every subsequent
	// failure after the first lockout until the client is forgiven.
	LockoutPerError bool
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
	if l.cfg.Global > 0 && len(l.recent) >= l.cfg.Global {
		return false, l.recent[0].Add(l.cfg.Window).Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt from addr.
func (l *Limiter) Fail(addr netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.cfg.Global > 0 {
		l.recent = append(l.recent, now)
	}
	key := clientKey(addr)
	c := l.clients[key]
	if c == nil {
		if len(l.clients) >= maxTrackedClients {
			l.lastPurge = time.Time{}
			l.purgeLocked(now)
			if len(l.clients) >= maxTrackedClients {
				l.evictOneLocked(now)
			}
		}
		c = &clientFailures{}
		l.clients[key] = c
	} else if c.strikes > 0 && now.After(c.until.Add(l.cfg.MaxLockout)) {
		c.strikes = 0
		c.until = time.Time{}
	}
	if !(l.cfg.LockoutPerError && c.strikes > 0) {
		c.times = append(trimBefore(c.times, now.Add(-l.cfg.Window)), now)
		if len(c.times) < l.cfg.PerClient {
			return
		}
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
// window.
func (l *Limiter) purgeLocked(now time.Time) {
	if l.cfg.Global > 0 {
		l.recent = trimBefore(l.recent, now.Add(-l.cfg.Window))
	}
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

// evictOneLocked removes a single tracked client to keep memory bounded
// when Global == 0; unlocked entries or earliest expirations go first.
func (l *Limiter) evictOneLocked(now time.Time) {
	var victim netip.Prefix
	var victimUntil time.Time
	first := true
	for key, c := range l.clients {
		if !now.Before(c.until) {
			delete(l.clients, key)
			return
		}
		if first || c.until.Before(victimUntil) {
			victim, victimUntil, first = key, c.until, false
		}
	}
	if !first {
		delete(l.clients, victim)
	}
}

func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}
