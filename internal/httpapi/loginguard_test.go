package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/auth"
)

const (
	addrLoopback = "127.0.0.1:54321"
	addrTunnel   = "127.0.0.1:54322" // cloudflared connects from this machine
	addrPublic   = "203.0.113.9:443"
)

// loginPost builds a POST /login the way a browser sends it. remoteAddr is the
// connection the server sees: the tunnel connects from loopback and names the
// real caller in CF-Connecting-IP.
func loginPost(code, remoteAddr string, header map[string]string, cookies ...*http.Cookie) *http.Request {
	form := url.Values{"totp": {code}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:20130"
	r.RemoteAddr = remoteAddr
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func send(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// guardCounts reads the three budgets. The caller must not hold the lock.
func guardCounts(g *loginGuard) (public, addresses, devices int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.public), len(g.perIP), len(g.device)
}

// fillPublicBudget sends wrong codes from a new address each time, so only the
// public budget can stop them. It returns the handler's last status.
func fillPublicBudget(t *testing.T, h http.Handler) {
	t.Helper()
	for i := 0; i < loginPublicMax; i++ {
		rec := send(h, loginPost("000000", addrTunnel, map[string]string{
			"CF-Connecting-IP": fmt.Sprintf("198.51.100.%d", i%256),
		}))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("fill attempt %d: code=%d, want 401", i, rec.Code)
		}
	}
}

// T1-1 (1) and T7-5 (b): the public budget bounds how many codes a caller that
// rotates its address can have evaluated. The numbers come from the constants.
func TestLoginBruteForceBoundIsBelowOnePercent(t *testing.T) {
	const span = 30 * 24 * time.Hour
	windows := int(span / loginPublicWin)
	maxChecks := windows * loginPublicMax
	// Three time windows are accepted, so one guess matches with 3 of 10^6.
	const perGuess = 3e-6
	if maxChecks > 3000 {
		t.Errorf("%d evaluated codes in 30 days, want at most 3000", maxChecks)
	}
	if p := float64(maxChecks) * perGuess; p >= 0.01 {
		t.Errorf("%d codes in 30 days gives p=%.4f, want below 0.01", maxChecks, p)
	}
}

// T1-1 (2) and T7-5: the charge and the test happen under one lock, so a burst
// cannot go past the cap.
func TestLoginGuardReserveIsAtomicUnderLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		ipOf func(i int) string
		want int
	}{
		{"rotating addresses stop at the public cap", func(i int) string { return fmt.Sprintf("198.51.100.%d", i) }, loginPublicMax},
		{"one address stops at its own cap", func(int) string { return "198.51.100.7" }, loginPerIPMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newLoginGuard()
			now := time.Now()
			var admitted atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if ok, _ := g.reserve(loginSource{lane: lanePublic, ip: tc.ipOf(i)}, now); ok {
						admitted.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if got := int(admitted.Load()); got != tc.want {
				t.Fatalf("admitted %d of 200 attempts, want %d", got, tc.want)
			}
		})
	}
}

// T1-1 (2) and T7-5: 200 concurrent wrong codes through the real handler. A 401
// means the code was evaluated; a 429 means the guard stopped the request
// before that, so the count of 401 answers is the count of code checks.
func TestLoginHandlerEvaluatesNoMoreCodesThanTheCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfOf func(i int) string
		cap  int
	}{
		{"rotating addresses", func(i int) string { return fmt.Sprintf("198.51.100.%d", i) }, loginPublicMax},
		{"one address", func(int) string { return "198.51.100.7" }, loginPerIPMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			h := NewWithAuth(s, nil, authConfig())
			var evaluated, refused atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					rec := send(h, loginPost("000000", addrTunnel, map[string]string{
						"CF-Connecting-IP": tc.cfOf(i),
					}))
					switch rec.Code {
					case http.StatusUnauthorized:
						evaluated.Add(1)
					case http.StatusTooManyRequests:
						refused.Add(1)
					default:
						t.Errorf("attempt %d: code=%d, want 401 or 429", i, rec.Code)
					}
				}(i)
			}
			wg.Wait()
			if got := int(evaluated.Load()); got > tc.cap {
				t.Fatalf("%d codes evaluated, want at most %d", got, tc.cap)
			}
			if evaluated.Load()+refused.Load() != 200 {
				t.Fatalf("answers: %d evaluated + %d refused, want 200", evaluated.Load(), refused.Load())
			}
		})
	}
}

// T1-1 (3): wrong codes below the cap never block the correct one.
func TestLoginAcceptsACorrectCodeAfterFourWrongOnes(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	head := map[string]string{"CF-Connecting-IP": "198.51.100.30"}
	for i := 0; i < loginPerIPMax-1; i++ {
		if rec := send(h, loginPost("000000", addrTunnel, head)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: code=%d, want 401", i, rec.Code)
		}
	}
	rec := send(h, loginPost(auth.TOTPNow(cfg.TOTPSecret), addrTunnel, head))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("correct code after %d wrong ones: code=%d loc=%q, want 302 -> /",
			loginPerIPMax-1, rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(rec, sessionCookie) == nil {
		t.Fatal("the correct code set no session cookie")
	}
}

// T1-1 (4): a correct code gives its charge back, so it leaves nothing behind.
func TestSuccessfulLoginLeavesNoEntryInAnyBudget(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)
	rec := send(h, loginPost(auth.TOTPNow(cfg.TOTPSecret), addrTunnel,
		map[string]string{"CF-Connecting-IP": "198.51.100.31"}))
	if rec.Code != http.StatusFound {
		t.Fatalf("login code=%d, want 302", rec.Code)
	}
	public, addresses, devices := guardCounts(a.login)
	if public != 0 || addresses != 0 || devices != 0 {
		t.Fatalf("after a correct code: public=%d addresses=%d devices=%d, want 0 0 0", public, addresses, devices)
	}
}

// T1-2: a stranger who fills the public budget cannot keep the owner out. The
// owner path is a loopback connection with no forwarding header.
func TestAFullPublicBudgetNeverLocksOutTheOwnerPath(t *testing.T) {
	t.Setenv("INTACT_OWNER_LOOPBACK", "1") // the owner lane exists only with the opt-in
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	fillPublicBudget(t, h)

	// The budget is full: one more public attempt never reaches the code.
	full := send(h, loginPost("000000", addrTunnel, map[string]string{"CF-Connecting-IP": "198.51.100.200"}))
	if full.Code != http.StatusTooManyRequests {
		t.Fatalf("public attempt with a full budget: code=%d, want 429", full.Code)
	}

	// (b) a correct code that arrives through the tunnel still gets 429.
	code := auth.TOTPNow(cfg.TOTPSecret)
	tun := send(h, loginPost(code, addrTunnel, map[string]string{"CF-Connecting-IP": "198.51.100.201"}))
	if tun.Code != http.StatusTooManyRequests {
		t.Fatalf("correct code from the tunnel with a full budget: code=%d, want 429", tun.Code)
	}

	// (a) the same code on the owner path signs in.
	own := send(h, loginPost(code, addrLoopback, nil))
	if own.Code != http.StatusFound || own.Header().Get("Location") != "/" {
		t.Fatalf("owner path with a full budget: code=%d loc=%q, want 302 -> /", own.Code, own.Header().Get("Location"))
	}
	if cookieNamed(own, sessionCookie) == nil {
		t.Fatal("the owner path set no session cookie")
	}
}

// The device lane: a browser that signed in before keeps its own budget, so a
// full public budget does not reach it. A forged cookie names no device.
func TestDeviceCookieSignsInWhileThePublicBudgetIsFull(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	fillPublicBudget(t, h)
	code := auth.TOTPNow(cfg.TOTPSecret)
	head := map[string]string{"CF-Connecting-IP": "198.51.100.210"}

	forged := &http.Cookie{Name: deviceCookie, Value: "0123456789abcdef.not-a-real-signature"}
	if rec := send(h, loginPost(code, addrTunnel, head, forged)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("forged device cookie: code=%d, want 429 (the public lane)", rec.Code)
	}

	known := &http.Cookie{Name: deviceCookie, Value: cfg.IssueDevice()}
	rec := send(h, loginPost(code, addrTunnel, head, known))
	if rec.Code != http.StatusFound {
		t.Fatalf("known device with a full public budget: code=%d, want 302", rec.Code)
	}
	if cookieNamed(rec, sessionCookie) == nil {
		t.Fatal("the device lane set no session cookie")
	}
}

func TestSuccessfulLoginSetsADeviceCookie(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	rec := send(h, loginPost(auth.TOTPNow(cfg.TOTPSecret), addrLoopback, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login code=%d, want 302", rec.Code)
	}
	ck := cookieNamed(rec, deviceCookie)
	if ck == nil {
		t.Fatal("login set no device cookie")
	}
	if _, ok := cfg.DeviceID(ck.Value); !ok {
		t.Errorf("the device cookie %q does not carry a valid signature", ck.Value)
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.MaxAge != deviceMaxAge {
		t.Errorf("device cookie flags: HttpOnly=%v SameSite=%v MaxAge=%d, want true Strict %d",
			ck.HttpOnly, ck.SameSite, ck.MaxAge, deviceMaxAge)
	}
	// A plain http request over loopback must not ask for Secure, or the
	// browser drops the cookie.
	if ck.Secure {
		t.Error("device cookie is Secure on a plain loopback request")
	}
}

// A browser keeps the device it already has, so its budget cannot be reset by
// signing in again.
func TestLoginKeepsAKnownDeviceCookie(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	known := &http.Cookie{Name: deviceCookie, Value: cfg.IssueDevice()}
	rec := send(h, loginPost(auth.TOTPNow(cfg.TOTPSecret), addrLoopback, nil, known))
	if rec.Code != http.StatusFound {
		t.Fatalf("login code=%d, want 302", rec.Code)
	}
	if ck := cookieNamed(rec, deviceCookie); ck == nil || ck.Value != known.Value {
		t.Fatalf("device cookie changed on sign-in: %v", ck)
	}
}

// One attempt is charged to one lane only: a device pays from its own budget.
func TestDeviceLaneHasItsOwnBudget(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)
	known := &http.Cookie{Name: deviceCookie, Value: cfg.IssueDevice()}
	head := map[string]string{"CF-Connecting-IP": "198.51.100.220"}
	for i := 0; i < loginDeviceMax; i++ {
		if rec := send(h, loginPost("000000", addrTunnel, head, known)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("device attempt %d: code=%d, want 401", i, rec.Code)
		}
	}
	if rec := send(h, loginPost("000000", addrTunnel, head, known)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d from one device: code=%d, want 429", loginDeviceMax+1, rec.Code)
	}
	public, addresses, devices := guardCounts(a.login)
	if public != 0 || addresses != 0 {
		t.Errorf("device failures reached another budget: public=%d addresses=%d, want 0 0", public, addresses)
	}
	if devices != 1 {
		t.Errorf("device budgets=%d, want 1", devices)
	}
}

// The owner path is never charged, so nothing an attacker does can fill it.
func TestLocalLaneIsNeverCharged(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if ok, _ := g.reserve(loginSource{lane: laneLocal}, now); !ok {
			t.Fatalf("the local lane refused attempt %d", i)
		}
	}
	if public, addresses, devices := guardCounts(g); public != 0 || addresses != 0 || devices != 0 {
		t.Fatalf("local attempts were charged: public=%d addresses=%d devices=%d", public, addresses, devices)
	}
}

func TestLoginGuardBlocksOneAddressAndClearsItsWindow(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	src := loginSource{lane: lanePublic, ip: "198.51.100.40"}
	for i := 0; i < loginPerIPMax; i++ {
		if ok, _ := g.reserve(src, now); !ok {
			t.Fatalf("attempt %d refused too early", i)
		}
	}
	ok, wait := g.reserve(src, now)
	if ok || wait <= 0 {
		t.Fatalf("address not blocked after %d failures: ok=%v wait=%v", loginPerIPMax, ok, wait)
	}
	// Another address still has its own budget.
	if ok, _ := g.reserve(loginSource{lane: lanePublic, ip: "198.51.100.41"}, now); !ok {
		t.Fatal("a second address was blocked by the first's failures")
	}
	// The window clears with time.
	if ok, _ := g.reserve(src, now.Add(loginPerIPWin+time.Second)); !ok {
		t.Fatal("the per-address window did not clear")
	}
}

func TestPublicWindowClearsAfterItsSpan(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for i := 0; i < loginPublicMax; i++ {
		g.reserve(loginSource{lane: lanePublic, ip: fmt.Sprintf("198.51.100.%d", i)}, now)
	}
	fresh := loginSource{lane: lanePublic, ip: "203.0.113.5"}
	if ok, _ := g.reserve(fresh, now); ok {
		t.Fatal("the public budget admitted an attempt past its cap")
	}
	if ok, _ := g.reserve(fresh, now.Add(loginPublicWin+time.Second)); !ok {
		t.Fatal("the public window did not clear after its span")
	}
}

// The refund gives back one charge, never the whole budget: a stranger's
// failures stay counted after the owner signs in.
func TestRefundReturnsOnlyItsOwnCharge(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for i := 0; i < 3; i++ {
		g.reserve(loginSource{lane: lanePublic, ip: fmt.Sprintf("198.51.100.%d", i)}, now)
	}
	mine := loginSource{lane: lanePublic, ip: "203.0.113.6"}
	if ok, _ := g.reserve(mine, now); !ok {
		t.Fatal("reserve refused a free budget")
	}
	g.refund(mine)
	if public, addresses, _ := guardCounts(g); public != 3 || addresses != 3 {
		t.Fatalf("after a refund: public=%d addresses=%d, want 3 3", public, addresses)
	}
}

func TestLoginSourceOfPicksOneLane(t *testing.T) {
	cfg := authConfig()
	device := cfg.IssueDevice()
	id, _ := cfg.DeviceID(device)

	for _, tc := range []struct {
		name       string
		remoteAddr string
		header     map[string]string
		cookie     string
		want       loginSource
	}{
		{"loopback with no header is the owner path", addrLoopback, nil, "", loginSource{lane: laneLocal}},
		{"ipv6 loopback is the owner path", "[::1]:41000", nil, "", loginSource{lane: laneLocal}},
		{"a tunnel request is public", addrTunnel, map[string]string{"CF-Connecting-IP": "9.9.9.9"}, "",
			loginSource{lane: lanePublic, ip: "9.9.9.9"}},
		{"a forwarded header alone takes the owner path away", addrTunnel,
			map[string]string{"X-Forwarded-For": "9.9.9.8"}, "", loginSource{lane: lanePublic, ip: "9.9.9.8"}},
		{"a direct remote caller is public", addrPublic, nil, "", loginSource{lane: lanePublic, ip: "203.0.113.9"}},
		{"a known device gets its own lane", addrTunnel, map[string]string{"CF-Connecting-IP": "9.9.9.9"}, device,
			loginSource{lane: laneDevice, device: id}},
		{"a forged device cookie is public", addrTunnel, map[string]string{"CF-Connecting-IP": "9.9.9.9"}, "abc.def",
			loginSource{lane: lanePublic, ip: "9.9.9.9"}},
		{"a device cookie does not move the owner path", addrLoopback, nil, device, loginSource{lane: laneLocal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/login", nil)
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: deviceCookie, Value: tc.cookie})
			}
			if got := loginSourceOf(r, cfg.DeviceID, true); got != tc.want {
				t.Errorf("loginSourceOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// T8-4: which address a public attempt is charged to. A forwarding header is
// read only from a loopback connection, because only the tunnel writes it.
func TestClientIPPrefersCFThenXFFFromLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		cf, xff    string
		want       string
	}{
		{"CF wins over XFF", addrLoopback, "1.1.1.1", "2.2.2.2", "1.1.1.1"},
		{"the first XFF entry, trimmed", addrLoopback, "", " 2.2.2.2 , 3.3.3.3", "2.2.2.2"},
		{"one XFF entry", addrLoopback, "", "4.4.4.4", "4.4.4.4"},
		{"no header is the connection", "5.5.5.5:1234", "", "", "5.5.5.5"},
		{"ipv6 loopback", "[::1]:80", "", "", "::1"},
		{"a remote caller cannot name itself with CF", "5.5.5.5:1234", "1.1.1.1", "", "5.5.5.5"},
		{"a remote caller cannot name itself with XFF", "5.5.5.5:1234", "", "2.2.2.2", "5.5.5.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/login", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.cf != "" {
				r.Header.Set("CF-Connecting-IP", tc.cf)
			}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A TOTP code opens one session. A second use inside the same window gets the
// wrong-code answer.
func TestTOTPCodeCannotBeReplayed(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	code := auth.TOTPNow(cfg.TOTPSecret)
	if rec := send(h, loginPost(code, addrLoopback, nil)); rec.Code != http.StatusFound {
		t.Fatalf("first use: code=%d, want 302", rec.Code)
	}
	rec := send(h, loginPost(code, addrLoopback, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second use of the same code: code=%d, want 401", rec.Code)
	}
	if cookieNamed(rec, sessionCookie) != nil {
		t.Error("a replayed code opened a session")
	}
}

// A sign-in body carries one 6-digit field. Anything larger is refused before
// the form is read.
func TestLoginBodyIsCapped(t *testing.T) {
	s := openStore(t)
	h := NewWithAuth(s, nil, authConfig())
	body := "totp=" + strings.Repeat("1", loginBodyMax+1)
	r := httptest.NewRequest("POST", "/login", strings.NewReader(body))
	r.Host = "127.0.0.1:20130"
	r.RemoteAddr = addrLoopback
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := send(h, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized login body: code=%d, want 400", rec.Code)
	}
}

// C2 (round 2): without the opt-in, a loopback caller with no forwarding header
// is public. A proxy that adds no header makes every internet caller look local.
func TestLoopbackWithoutHeaderIsPublicByDefault(t *testing.T) {
	t.Setenv("INTACT_OWNER_LOOPBACK", "")
	h := NewWithAuth(openStore(t), nil, authConfig())
	for i := 1; i <= loginPerIPMax; i++ {
		if rec := send(h, loginPost("000000", addrLoopback, nil)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: status %d, want 401", i, rec.Code)
		}
	}
	if rec := send(h, loginPost("000000", addrLoopback, nil)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("wrong code %d: status %d, want 429", loginPerIPMax+1, rec.Code)
	}
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = addrLoopback
	if got := loginSourceOf(r, nil, false); got.lane != lanePublic {
		t.Fatalf("lane = %v, want lanePublic", got.lane)
	}
}

// C2 (round 2): INTACT_OWNER_LOOPBACK=1 gives the owner's unmetered lane back.
func TestOwnerLoopbackOptInRestoresLocalLane(t *testing.T) {
	t.Setenv("INTACT_OWNER_LOOPBACK", "1")
	h := NewWithAuth(openStore(t), nil, authConfig())
	for i := 1; i <= 3*loginPerIPMax; i++ {
		if rec := send(h, loginPost("000000", addrLoopback, nil)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: status %d, want 401", i, rec.Code)
		}
	}
}
