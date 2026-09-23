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

func TestAccountsListsWithoutSecrets(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	if _, err := s.CreateConnection("groq", "work", "gsk-do-not-leak"); err != nil {
		t.Fatal(err)
	}

	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/accounts", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "gsk-do-not-leak") {
		t.Fatalf("the credential reached the response: %s", rec.Body.String())
	}
	var got struct {
		Accounts []store.Connection `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Provider != "groq" {
		t.Errorf("accounts = %+v", got.Accounts)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// An empty store must answer with an empty list, not with null, so the dashboard
// can iterate the result without a special case.
func TestAccountsReturnsEmptyListNotNull(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()

	rec := httptest.NewRecorder()
	New(s, nil).ServeHTTP(rec, loopbackRequest("GET", "/accounts", nil))

	if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Errorf("body = %s, want an empty array", strings.TrimSpace(rec.Body.String()))
	}
}

func TestDashboardIsServedFromTheBinary(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<html", "Providers", "Endpoint", "/accounts", "/usage"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// "/" must serve the dashboard and nothing else. A bare ServeMux "/" pattern is a
// catch-all, which would answer an unknown path with the dashboard and hide typos.
func TestUnknownPathIsNotTheDashboard(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()

	rec := httptest.NewRecorder()
	New(s, nil).ServeHTTP(rec, loopbackRequest("GET", "/no-such-page", nil))

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<html") {
		t.Error("an unknown path returned the dashboard; the root route is a catch-all")
	}
}
