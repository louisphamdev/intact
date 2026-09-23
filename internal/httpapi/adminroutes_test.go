package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// adminAPIRoutes are the /api writes that change what intact calls, or how. A
// dashboard inference key must not reach any of them.
var adminAPIRoutes = []struct{ method, path, body string }{
	{"POST", "/api/accounts/no-such-account/active", `{"active":true}`},
	{"POST", "/api/accounts/no-such-account/test", `{}`},
	{"POST", "/api/providers/groq/rotation", `{"mode":"fallback"}`},
	{"POST", "/api/providers/groq/model-policy", `{"autoTest":false,"onlyFree":true}`},
	{"POST", "/api/rankings/refresh", ``},
	{"POST", "/api/rankings/alias", `{"provider":"groq","model":"llama","name":"-"}`},
	{"POST", "/api/providers/groq/models/active", `{"models":["llama"],"active":true}`},
	{"POST", "/api/providers/groq/models/delete", `{"models":["llama"]}`},
	{"POST", "/api/providers/groq/models/test", `{"model":"llama"}`},
}

// sendAs runs one request with a bearer token and returns the status code.
func sendAs(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := loopbackRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminAPIRoutesRefuseADashboardKey(t *testing.T) {
	// The leaderboard refresh must not leave this machine during a test.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rows":[]}`))
	}))
	defer up.Close()
	old := arenaAPI
	arenaAPI = up.URL
	defer func() { arenaAPI = old }()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)
	k, err := s.CreateAPIKey("laptop")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	for _, rt := range adminAPIRoutes {
		if rec := sendAs(h, rt.method, rt.path, rt.body, k.Key); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a dashboard key: code=%d body=%s, want 403", rt.path, rec.Code, rec.Body.String())
		}
	}
	// No refused call reached its handler.
	if pol := a.modelPolicy("groq"); pol.OnlyFree {
		t.Error("a dashboard key set the model policy")
	}
	if rot := a.rotation("groq"); rot.Mode == RotateFallback {
		t.Error("a dashboard key set the rotation")
	}
	for _, rt := range adminAPIRoutes {
		if rec := sendAs(h, rt.method, rt.path, rt.body, cfg.APIToken); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s with the master token: code=%d body=%s, want any other status", rt.path, rec.Code, rec.Body.String())
		}
	}
}
