package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/store"
)

func TestFiltersApplyToTheOutgoingRequest(t *testing.T) {
	var gotBody, gotBeta, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotBeta, gotAuth = string(b), r.Header.Get("Anthropic-Beta"), r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "c", "tok")
	h := New(s, map[string]string{"claude": up.URL})

	for _, f := range []string{
		`{"provider":"claude","kind":"field","pattern":"context_management","enabled":true}`,
		`{"provider":"*","kind":"header","pattern":"anthropic-beta","enabled":true}`,
		`{"provider":"*","kind":"header","pattern":"authorization","enabled":true}`,
		`{"provider":"groq","kind":"field","pattern":"temperature","enabled":true}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/filters", strings.NewReader(f)))
		if rec.Code != http.StatusOK {
			t.Fatalf("save %s: code=%d body=%s", f, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude/x","context_management":{"a":1},"temperature":0.5,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotBody, "context_management") || !strings.Contains(gotBody, `"temperature"`) ||
		!strings.Contains(gotBody, `"model":"x"`) {
		t.Errorf("body = %s, want the field gone and groq's rule not applied", gotBody)
	}
	if gotBeta != "" {
		t.Errorf("Anthropic-Beta = %q, want it dropped", gotBeta)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, the credential must never be dropped", gotAuth)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/filters", strings.NewReader(`{"kind":"system","pattern":"("}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad regex accepted: code=%d", rec.Code)
	}
}

// A field rule that strips the whole body or an essential field is refused, so a
// caller cannot install one that breaks every request.
func TestFilterRejectsBodyWipingPatterns(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	for _, pat := range []string{"*", "messages", "model", "input"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/filters",
			strings.NewReader(`{"provider":"*","kind":"field","pattern":"`+pat+`","enabled":true}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("pattern %q accepted: code=%d", pat, rec.Code)
		}
	}
	// A deeper, non-essential path is still allowed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/filters",
		strings.NewReader(`{"provider":"*","kind":"field","pattern":"messages.*.cache_control","enabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Errorf("safe deep path refused: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDefaultFiltersAreSeededOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, _ := store.Open(path)
	list, _ := s.ListFilters()
	if len(list) == 0 {
		t.Fatal("no default filters")
	}
	s.DeleteFilter(list[0].ID)
	s.Close()
	s, _ = store.Open(path)
	defer s.Close()
	again, _ := s.ListFilters()
	if len(again) != len(list)-1 {
		t.Errorf("filters after reopen = %d, want %d: a deleted default came back", len(again), len(list)-1)
	}
}

// saveFilter (the dashboard and the API) answers 400 for the same rules that
// putFilter (MCP, and both reviews) refuses.
func TestSaveFilterAndPutFilterShareTheGuard(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, h := newServer(s, nil, nil)
	post := func(kind, pattern string) int {
		rec := httptest.NewRecorder()
		body := `{"provider":"*","kind":"` + kind + `","pattern":` + quote(pattern) + `,"enabled":true}`
		h.ServeHTTP(rec, loopbackRequest("POST", "/filters", strings.NewReader(body)))
		return rec.Code
	}
	for _, p := range refusedFieldRules {
		if code := post(filter.Field, p); code != http.StatusBadRequest {
			t.Errorf("POST field %q = %d, want 400", p, code)
		}
		if _, err := a.putFilter(store.Filter{Provider: "*", Kind: filter.Field, Pattern: p, Enabled: true}); err == nil {
			t.Errorf("putFilter field %q was accepted", p)
		}
	}
	for _, re := range refusedSystemRules {
		if code := post(filter.System, re); code != http.StatusBadRequest {
			t.Errorf("POST system %q = %d, want 400", re, code)
		}
		if _, err := a.putFilter(store.Filter{Provider: "*", Kind: filter.System, Pattern: re, Enabled: true}); err == nil {
			t.Errorf("putFilter system %q was accepted", re)
		}
	}
	for _, p := range acceptedFieldRules {
		if code := post(filter.Field, p); code != http.StatusOK {
			t.Errorf("POST field %q = %d, want 200", p, code)
		}
	}
	for _, re := range acceptedSystemRules {
		if code := post(filter.System, re); code != http.StatusOK {
			t.Errorf("POST system %q = %d, want 200", re, code)
		}
	}
}
