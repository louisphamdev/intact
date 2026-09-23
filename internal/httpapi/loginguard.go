package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The login takes a 6-digit TOTP code. Without a limit, a caller can try codes
// as fast as the network allows; a valid code set (the current step and its
// neighbours) is small, so unlimited guessing breaks the only gate on the
// dashboard. loginGuard caps failed attempts per source and across all sources,
// so brute force needs far more time than a code stays valid.

const (
	loginPerIPMax   = 5                // failures from one source before it waits
	loginPerIPBlock = 15 * time.Minute // how long that source then waits
	loginGlobalMax  = 30               // failures from all sources within the window
	loginGlobalWin  = time.Minute      // the global window
)

type loginGuard struct {
	mu     sync.Mutex
	perIP  map[string]*ipFails
	global []time.Time // failure times inside the global window
}

type ipFails struct {
	count int
	until time.Time // blocked until this time when count reached the max
}

func newLoginGuard() *loginGuard { return &loginGuard{perIP: map[string]*ipFails{}} }

// allow reports whether a login attempt from ip may proceed. When it may not,
// it returns the time to wait before trying again.
func (g *loginGuard) allow(ip string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.perIP[ip]; s != nil && now.Before(s.until) {
		return false, s.until.Sub(now)
	}
	// Trim the global window, then refuse when it is already full: this stops a
	// brute force that rotates its source address.
	g.trimGlobal(now)
	if len(g.global) >= loginGlobalMax {
		return false, loginGlobalWin
	}
	return true, 0
}

// fail records a failed attempt from ip.
func (g *loginGuard) fail(ip string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.perIP[ip]
	if s == nil {
		s = &ipFails{}
		g.perIP[ip] = s
	}
	s.count++
	if s.count >= loginPerIPMax {
		s.until = now.Add(loginPerIPBlock)
		s.count = 0
	}
	g.trimGlobal(now)
	g.global = append(g.global, now)
}

// succeed clears the failure record for ip after a correct code.
func (g *loginGuard) succeed(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.perIP, ip)
}

// trimGlobal drops failure times older than the global window. The caller holds
// the lock.
func (g *loginGuard) trimGlobal(now time.Time) {
	cut := now.Add(-loginGlobalWin)
	i := 0
	for i < len(g.global) && g.global[i].Before(cut) {
		i++
	}
	g.global = g.global[i:]
}

// clientIP names the source of a request. Behind the Cloudflare tunnel the real
// address is in CF-Connecting-IP; the global cap covers a forged X-Forwarded-For.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
