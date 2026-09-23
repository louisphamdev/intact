package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// postKey runs one /keys/{id}/… call. A nil cookie sends no session.
func postKey(h http.Handler, path, body string, ck *http.Cookie, token string) *httptest.ResponseRecorder {
	req := loopbackRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The three key routes are the dashboard's only way to read a full API key, so
// each one must stay behind the session.
func TestKeyRoutesNeedTheSession(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)
	k := keyThroughDashboard(t, h, ck, "laptop")

	paths := []string{"/keys/" + k.ID + "/reveal", "/keys/" + k.ID + "/active", "/keys/" + k.ID + "/delete"}
	bodies := []string{"", `{"active":false}`, ""}

	for i, p := range paths {
		rec := postKey(h, p, bodies[i], nil, "")
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s with no session: code=%d loc=%q, want 302 -> /login", p, rec.Code, rec.Header().Get("Location"))
		}
		if rec := postKey(h, p, bodies[i], nil, cfg.APIToken); rec.Code == http.StatusOK {
			t.Errorf("%s answered the master token", p)
		}
		if rec := postKey(h, p, bodies[i], nil, k.Key); rec.Code == http.StatusOK {
			t.Errorf("%s answered a dashboard key", p)
		}
	}

	rec := postKey(h, paths[0], "", ck, "")
	var got struct {
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Key != k.Key || !strings.HasPrefix(got.Key, "sk-intact-") {
		t.Errorf("reveal with the session: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postKey(h, paths[1], `{"active":false}`, ck, ""); rec.Code != http.StatusOK {
		t.Errorf("active with the session: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if s.ValidAPIKey(k.Key) {
		t.Error("the key still works after it was switched off")
	}
	if rec := postKey(h, paths[2], "", ck, ""); rec.Code != http.StatusOK {
		t.Errorf("delete with the session: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if list, _ := s.ListAPIKeys(); len(list) != 0 {
		t.Errorf("the key was not deleted: %+v", list)
	}
}
