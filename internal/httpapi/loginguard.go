package httpapi

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The login takes a 6-digit TOTP code and nothing else, and three time windows
// are accepted, so one guess matches with p = 3/10^6. The public budget admits
// at most loginPublicMax wrong codes per loginPublicWin: 100 a day, so 3000 in
// 30 days and p = 0.009 for a caller that rotates its address. Every attempt is
// charged before the code is checked, so a concurrent burst cannot pass the cap.
const (
	loginPerIPMax  = 5                // wrong codes from one public address ...
	loginPerIPWin  = 15 * time.Minute // ... inside this window
	loginDeviceMax = 10               // wrong codes from one known browser ...
	loginDeviceWin = 24 * time.Hour   // ... inside this window
	loginPublicMax = 100              // wrong codes from all public addresses ...
	loginPublicWin = 24 * time.Hour   // ... inside this rolling window
)

// deviceCookie marks a browser that signed in before. It grants no access.
const deviceCookie = "intact_device"

// loginLane is the budget that pays for an attempt. An attempt has exactly one.
type loginLane int

const (
	// laneLocal is a client on this machine that sends no forwarding header, and
	// only when INTACT_OWNER_LOOPBACK=1. It is off by default: a front proxy that
	// adds no forwarding header makes every internet caller look like this one.
	laneLocal loginLane = iota
	// laneDevice is a browser that signed in before and kept its cookie.
	laneDevice
	// lanePublic is everything else.
	lanePublic
)

// loginSource names the one budget an attempt is charged to.
type loginSource struct {
	lane   loginLane
	ip     string // public lane only
	device string // device lane only
}

type loginGuard struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time // wrong codes per public address
	device map[string][]time.Time // wrong codes per known browser
	public []time.Time            // wrong codes from all public addresses
	// ownerLoopback enables laneLocal. Set it only when every proxy in front of
	// intact names the caller in a forwarding header.
	ownerLoopback bool
}

func newLoginGuard() *loginGuard {
	return &loginGuard{perIP: map[string][]time.Time{}, device: map[string][]time.Time{},
		ownerLoopback: os.Getenv("INTACT_OWNER_LOOPBACK") == "1"}
}

// reserve charges one attempt to the source's budget BEFORE the code is checked.
// The charge and the test happen under one lock, so a burst of requests cannot
// get more code checks than the cap. A correct code gives the charge back with
// refund. When the answer is false, the duration is the wait before a retry.
func (g *loginGuard) reserve(src loginSource, now time.Time) (bool, time.Duration) {
	if src.lane == laneLocal {
		return true, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if src.lane == laneDevice {
		win := trimWindow(g.device[src.device], now, loginDeviceWin)
		if len(win) >= loginDeviceMax {
			g.device[src.device] = win
			return false, windowWait(win, now, loginDeviceWin)
		}
		g.device[src.device] = append(win, now)
		return true, 0
	}
	perIP := trimWindow(g.perIP[src.ip], now, loginPerIPWin)
	if len(perIP) >= loginPerIPMax {
		g.store(src.ip, perIP)
		return false, windowWait(perIP, now, loginPerIPWin)
	}
	public := trimWindow(g.public, now, loginPublicWin)
	if len(public) >= loginPublicMax {
		g.public = public
		g.store(src.ip, perIP)
		return false, windowWait(public, now, loginPublicWin)
	}
	g.perIP[src.ip] = append(perIP, now)
	g.public = append(public, now)
	return true, 0
}

// refund gives back the charge of a correct code. The public budget gives back
// one charge and not the whole window, so a stranger's failures stay counted.
func (g *loginGuard) refund(src loginSource) {
	if src.lane == laneLocal {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if src.lane == laneDevice {
		delete(g.device, src.device)
		return
	}
	delete(g.perIP, src.ip)
	if n := len(g.public); n > 0 {
		g.public = g.public[:n-1]
	}
}

// store keeps an address's window, and drops the key when the window is empty
// so a source that stopped trying does not stay in the map. The caller holds
// the lock.
func (g *loginGuard) store(ip string, win []time.Time) {
	if len(win) == 0 {
		delete(g.perIP, ip)
		return
	}
	g.perIP[ip] = win
}

// trimWindow drops the attempts that left the window.
func trimWindow(times []time.Time, now time.Time, win time.Duration) []time.Time {
	cut := now.Add(-win)
	i := 0
	for i < len(times) && !times[i].After(cut) {
		i++
	}
	return times[i:]
}

// windowWait is the time until the oldest attempt in a full window expires.
func windowWait(times []time.Time, now time.Time, win time.Duration) time.Duration {
	if len(times) == 0 {
		return win
	}
	if d := times[0].Add(win).Sub(now); d > 0 {
		return d
	}
	return time.Second
}

// loginSourceOf decides which budget pays for an attempt. It reads the
// connection, the presence of the forwarding headers and the device cookie's
// signature. It never reads the code and never trusts a header's value, so a
// stranger cannot take the owner's lane and the owner cannot lose it.
func loginSourceOf(r *http.Request, deviceID func(string) (string, bool), ownerLoopback bool) loginSource {
	forwarded := r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != ""
	if ownerLoopback && !forwarded && loopbackAddr(remoteHost(r)) {
		return loginSource{lane: laneLocal}
	}
	if deviceID != nil {
		if ck, err := r.Cookie(deviceCookie); err == nil {
			if id, ok := deviceID(ck.Value); ok {
				return loginSource{lane: laneDevice, device: id}
			}
		}
	}
	return loginSource{lane: lanePublic, ip: clientIP(r)}
}

// clientIP names the public source of a request. Behind the Cloudflare tunnel
// the real address is in CF-Connecting-IP, and cloudflared connects from this
// machine, so a forwarding header counts only on a loopback connection. A
// caller that reaches the port directly cannot choose its own bucket.
func clientIP(r *http.Request) string {
	addr := remoteHost(r)
	if !loopbackAddr(addr) {
		return addr
	}
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	return addr
}

// remoteHost is the address of the connection, without its port.
func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// loopbackAddr reports whether a connection address is on this machine. It
// reads an address, not a Host header, so it accepts the whole 127.0.0.0/8
// range. Use loopbackHost for a Host header, which answers another question.
func loopbackAddr(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}
