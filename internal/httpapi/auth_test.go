package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/store"
)

func authConfig() *auth.Config {
	return auth.NewConfig("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "machine-tok", []byte("k"), 3600)
}

func TestBrowserRoutesRequireSession(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := NewWithAuth(s, nil, authConfig())

	for _, path := range []string{"/", "/accounts", "/usage"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s without session: code=%d loc=%q, want 302 -> /login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestLoginWithCodeThenReachDashboard(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	form := url.Values{"totp": {auth.TOTPNow(cfg.TOTPSecret)}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login POST code=%d, want 302", rec.Code)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("login set no cookie")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/accounts", nil)
	req2.AddCookie(cookie[0])
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("with session /accounts code=%d, want 200", rec2.Code)
	}
}

func TestLoginRejectsWrongCode(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := NewWithAuth(s, nil, authConfig())
	form := url.Values{"totp": {"000000"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound || len(rec.Result().Cookies()) > 0 {
		t.Error("wrong code produced a session")
	}
}

func TestProxyRequiresBearerToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "x", "gsk")
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, authConfig())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"groq/m"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: code=%d, want 401", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"groq/m"}`))
	req2.Header.Set("Authorization", "Bearer machine-tok")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("with bearer: code=%d, want 200", rec2.Code)
	}
}

// Regression: the login page CSS contains literal % (keyframes), so it must not
// be run through fmt.Sprintf. The rendered page must have no format error and no
// leftover placeholder.
func TestLoginPageRendersCleanly(t *testing.T) {
	for _, msg := range []string{"", "Wrong or expired code. Try again."} {
		page := renderLogin(msg)
		if strings.Contains(page, "%!") || strings.Contains(page, "(MISSING)") {
			t.Errorf("login page has a format error with msg=%q", msg)
		}
		if strings.Contains(page, "{{ERR}}") {
			t.Errorf("login page left a placeholder unfilled with msg=%q", msg)
		}
		if !strings.Contains(page, `aria-label="digit 1"`) {
			t.Errorf("login page missing the OTP inputs")
		}
		if msg != "" && !strings.Contains(page, msg) {
			t.Errorf("login page did not show the error message")
		}
	}
}
