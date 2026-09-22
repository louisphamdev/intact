package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestParseCallback(t *testing.T) {
	for in, want := range map[string][2]string{
		"http://localhost:1455/auth/callback?code=abc&scope=x&state=s1": {"abc", "s1"},
		"abc#s2": {"abc", "s2"},
		" abc  ": {"abc", ""},
	} {
		c, s := parseCallback(in)
		if c != want[0] || s != want[1] {
			t.Errorf("parseCallback(%q) = %q %q", in, c, s)
		}
	}
}

func TestLoginStartBuildsThePKCEURL(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/codex/start", nil))
	var d struct{ URL, State string }
	json.Unmarshal(rec.Body.Bytes(), &d)
	u, err := url.Parse(d.URL)
	if err != nil || u.Host != "auth.openai.com" {
		t.Fatalf("url = %s", d.URL)
	}
	q := u.Query()
	if q.Get("redirect_uri") != "http://localhost:1455/auth/callback" || q.Get("code_challenge_method") != "S256" ||
		q.Get("state") != d.State || q.Get("originator") != "codex_cli_rs" {
		t.Errorf("query = %v", q)
	}
	// A pasted URL from another sign-in is refused before any exchange.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/codex/finish",
		strings.NewReader(`{"state":"`+d.State+`","input":"http://localhost:1455/auth/callback?code=x&state=other"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mismatched state: code=%d", rec.Code)
	}
}
