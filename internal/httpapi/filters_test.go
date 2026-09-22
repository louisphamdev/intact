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
		`{"provider":"groq","kind":"field","pattern":"model","enabled":true}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/filters", strings.NewReader(f)))
		if rec.Code != http.StatusOK {
			t.Fatalf("save %s: code=%d body=%s", f, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude/x","context_management":{"a":1},"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotBody, "context_management") || !strings.Contains(gotBody, `"model":"x"`) {
		t.Errorf("body = %s, want the field gone and groq's rule not applied", gotBody)
	}
	if gotBeta != "" {
		t.Errorf("Anthropic-Beta = %q, want it dropped", gotBeta)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, the credential must never be dropped", gotAuth)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/filters", strings.NewReader(`{"kind":"system","pattern":"("}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad regex accepted: code=%d", rec.Code)
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
