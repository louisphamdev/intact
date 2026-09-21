package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestModelsForAccountReturnsProviderList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("upstream path=%q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "x", "gsk")
	cfg := authConfig()
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, cfg)
	ck := loginCookie(t, h, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/accounts/"+c.ID+"/models", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "m1") {
		t.Fatalf("models code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModelsNeedsSession(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "x", "gsk")
	h := NewWithAuth(s, nil, authConfig())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/"+c.ID+"/models", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("unauth models code=%d, want 302", rec.Code)
	}
}
