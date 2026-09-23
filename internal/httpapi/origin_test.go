package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/store"
)

// guardReq builds a request with an explicit Host, which httptest.NewRequest
// otherwise fixes at example.com.
func guardReq(method, target, host string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = host
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func serveGuard(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func defCount(t *testing.T, s *store.Store) int {
	t.Helper()
	defs, err := s.ProviderDefs()
	if err != nil {
		t.Fatalf("provider defs: %v", err)
	}
	return len(defs)
}

const evilDef = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"put_provider_def",` +
	`"arguments":{"def":{"id":"evil","name":"Evil","kind":"apikey","api":"openai","baseUrl":"https://evil.example/v1"}}}}`

const evilDefJSON = `{"id":"evil","name":"Evil","kind":"apikey","api":"openai","baseUrl":"https://evil.example/v1"}`

// T6-4 (1) and T7-4: with no auth the Host is the only thing that separates a
// browser on the operator's machine from a rebound DNS name.
func TestNoAuthRefusesAForeignHost(t *testing.T) {
	s := openStore(t)
	h := New(s, nil)

	for _, host := range []string{"evil.example", "evil.example:1234", "intact.example"} {
		if rec := serveGuard(h, guardReq("GET", "/accounts", host, nil)); rec.Code != http.StatusForbidden {
			t.Errorf("GET /accounts Host %q: code=%d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:20130", "localhost:20130", "127.0.0.1", "localhost", "[::1]:20130"} {
		if rec := serveGuard(h, guardReq("GET", "/accounts", host, nil)); rec.Code != http.StatusOK {
			t.Errorf("GET /accounts Host %q: code=%d, want 200", host, rec.Code)
		}
	}
}

// T3-2 (2): a rebound name reaching /mcp must not run an admin tool.
func TestNoAuthRefusesMCPFromAForeignHost(t *testing.T) {
	s := openStore(t)
	h := New(s, nil)
	before := defCount(t, s)

	r := guardReq("POST", "/mcp", "evil.example:1234", strings.NewReader(evilDef))
	r.Header.Set("Origin", "http://evil.example:1234")
	if rec := serveGuard(h, r); rec.Code != http.StatusForbidden {
		t.Errorf("POST /mcp from a foreign Host: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if after := defCount(t, s); after != before {
		t.Errorf("provider defs changed: %d -> %d", before, after)
	}
}

// T3-2 (1): a cross-site page posting to the loopback port must not run a tool.
func TestNoAuthRefusesMCPFromACrossSiteOrigin(t *testing.T) {
	s := openStore(t)
	h := New(s, nil)
	before := defCount(t, s)

	r := guardReq("POST", "/mcp", "127.0.0.1:20130", strings.NewReader(evilDef))
	r.Header.Set("Origin", "https://evil.example")
	if rec := serveGuard(h, r); rec.Code != http.StatusForbidden {
		t.Errorf("POST /mcp with a cross-site Origin: code=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if after := defCount(t, s); after != before {
		t.Errorf("provider defs changed: %d -> %d", before, after)
	}
}

// T7-4 and T6-4 (2): the Origin rule, in every shape the ruling names.
func TestOriginRuleOnAWriteRoute(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		fetch   string
		refused bool
	}{
		{name: "cross-site origin", origin: "https://evil.example", refused: true},
		{name: "opaque origin", origin: "null", refused: true},
		{name: "cross-site fetch metadata", fetch: "cross-site", refused: true},
		{name: "same host, other scheme", origin: "https://127.0.0.1:20130"},
		{name: "same origin", origin: "http://127.0.0.1:20130"},
		{name: "no origin"},
		{name: "same-origin fetch metadata", origin: "http://127.0.0.1:20130", fetch: "same-origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t)
			h := New(s, nil)
			before := defCount(t, s)

			r := guardReq("POST", "/provider-defs", "127.0.0.1:20130", strings.NewReader(evilDefJSON))
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.fetch != "" {
				r.Header.Set("Sec-Fetch-Site", c.fetch)
			}
			rec := serveGuard(h, r)
			after := defCount(t, s)
			if c.refused {
				if rec.Code != http.StatusForbidden {
					t.Errorf("code=%d body=%s, want 403", rec.Code, rec.Body.String())
				}
				if after != before {
					t.Errorf("provider defs changed: %d -> %d", before, after)
				}
				return
			}
			if rec.Code == http.StatusForbidden {
				t.Errorf("code=403 body=%s, want the request through", rec.Body.String())
			}
			if after != before+1 {
				t.Errorf("provider defs = %d, want %d", after, before+1)
			}
		})
	}
}

// T7-4 and T6-4 (2): a GET carries no write, so the Origin rule leaves it alone.
func TestOriginRuleSkipsSafeMethods(t *testing.T) {
	s := openStore(t)
	h := New(s, nil)
	r := guardReq("GET", "/accounts", "127.0.0.1:20130", nil)
	r.Header.Set("Origin", "https://evil.example")
	if rec := serveGuard(h, r); rec.Code != http.StatusOK {
		t.Errorf("GET with a cross-site Origin: code=%d, want 200", rec.Code)
	}
}

// T7-4: with auth on, the Host allow-list does not apply, so the documented
// tunnel deployment keeps working.
func TestAuthOnKeepsAPublicHost(t *testing.T) {
	s := openStore(t)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	rec := serveGuard(h, guardReq("GET", "/accounts", "intact.example", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("GET /accounts with no session: code=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}

	form := url.Values{"totp": {auth.TOTPNow(cfg.TOTPSecret)}}
	lr := guardReq("POST", "/login", "intact.example", strings.NewReader(form.Encode()))
	lr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lr.Header.Set("Origin", "https://intact.example")
	login := serveGuard(h, lr)
	if login.Code != http.StatusFound {
		t.Fatalf("login: code=%d, want 302", login.Code)
	}
	session := cookieNamed(login, sessionCookie)
	if session == nil {
		t.Fatal("login set no session cookie")
	}

	pr := guardReq("POST", "/provider-defs", "intact.example", strings.NewReader(evilDefJSON))
	pr.Header.Set("Origin", "https://intact.example")
	pr.AddCookie(session)
	if rec := serveGuard(h, pr); rec.Code != http.StatusOK {
		t.Errorf("same-origin write over the tunnel: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// T7-4: with auth on, a cross-site Origin is still refused.
func TestAuthOnStillRefusesACrossSiteOrigin(t *testing.T) {
	s := openStore(t)
	h := NewWithAuth(s, nil, authConfig())
	r := guardReq("POST", "/provider-defs", "intact.example", strings.NewReader(evilDefJSON))
	r.Header.Set("Origin", "https://evil.example")
	if rec := serveGuard(h, r); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site write with auth on: code=%d, want 403", rec.Code)
	}
}

// T6-4 (3): the dashboard and the login page must not be framed.
func TestFramingHeadersOnEveryResponse(t *testing.T) {
	s := openStore(t)
	h := New(s, nil)
	for _, path := range []string{"/", "/login", "/accounts"} {
		rec := serveGuard(h, guardReq("GET", path, "127.0.0.1:20130", nil))
		for header, want := range map[string]string{
			"Content-Security-Policy": "frame-ancestors 'none'",
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}
