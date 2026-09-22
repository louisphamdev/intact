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

func loginCookie(t *testing.T, h http.Handler, cfg *auth.Config) *http.Cookie {
	t.Helper()
	form := url.Values{"totp": {auth.TOTPNow(cfg.TOTPSecret)}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	cks := rec.Result().Cookies()
	if len(cks) == 0 {
		t.Fatal("login gave no cookie")
	}
	return cks[0]
}

func TestAddAndDeleteConnectionThroughDashboard(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	// create
	form := url.Values{"provider": {"groq"}, "label": {"Work"}, "secret": {"gsk-xyz"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("create code=%d, want 302", rec.Code)
	}
	list, _ := s.ListConnections()
	if len(list) != 1 || list[0].Provider != "groq" || list[0].Label != "Work" {
		t.Fatalf("connection not created: %+v", list)
	}
	id := list[0].ID

	// delete
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/accounts/"+id+"/delete", nil)
	req2.AddCookie(ck)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusFound {
		t.Fatalf("delete code=%d, want 302", rec2.Code)
	}
	list2, _ := s.ListConnections()
	if len(list2) != 0 {
		t.Fatalf("connection not deleted: %+v", list2)
	}
}

func TestAddConnectionNeedsSession(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := NewWithAuth(s, nil, authConfig())
	form := url.Values{"provider": {"groq"}, "label": {"x"}, "secret": {"y"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("unauth create code=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
	if list, _ := s.ListConnections(); len(list) != 0 {
		t.Error("unauth create still added a connection")
	}
}

func TestToggleActiveThroughDashboard(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "a", "k")
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	req := httptest.NewRequest("POST", "/accounts/"+c.ID+"/active", strings.NewReader(`{"active":false}`))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle code=%d body=%s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListConnections()
	if list[0].IsActive {
		t.Error("connection still active after toggle")
	}
	// An inactive account is skipped by round-robin.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/groq/x", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Errorf("round-robin with no active account: code=%d", rec.Code)
	}
}
