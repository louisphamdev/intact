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

func TestProxyForwardsPathBodyAndAuthUnchanged(t *testing.T) {
	var gotPath, gotAuth, gotBody, gotQuery, gotCT string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"upstream":"said this"}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.CreateConnection("groq", "test", "gsk-abc")
	if err != nil {
		t.Fatal(err)
	}

	h := New(s, map[string]string{"groq": up.URL})
	body := `{"model":"llama-3.3-70b-versatile","messages":[]}`
	req := httptest.NewRequest("POST", "/p/"+c.ID+"/chat/completions?beta=1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", gotPath)
	}
	if gotQuery != "beta=1" {
		t.Errorf("query = %q, want beta=1: the query string must survive", gotQuery)
	}
	if gotBody != body {
		t.Errorf("body was altered:\n got %s\nwant %s", gotBody, body)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want it forwarded", gotCT)
	}
	if gotAuth != "Bearer gsk-abc" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if rec.Body.String() != `{"upstream":"said this"}` {
		t.Errorf("response was altered: %s", rec.Body.String())
	}
}

// A caller must never be able to override the credential this proxy attaches.
func TestProxyReplacesAnyIncomingAuthorization(t *testing.T) {
	var gotAuth string
	var authCount int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		authCount = len(r.Header.Values("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "test", "gsk-real")

	h := New(s, map[string]string{"groq": up.URL})
	req := httptest.NewRequest("POST", "/p/"+c.ID+"/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer attacker-supplied")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotAuth != "Bearer gsk-real" {
		t.Errorf("auth = %q, want the stored credential to win", gotAuth)
	}
	if authCount != 1 {
		t.Errorf("Authorization appeared %d times, want exactly 1", authCount)
	}
}

// A streamed response must reach the caller as it arrives, not after the upstream
// has finished. Buffering would break every SSE client.
func TestProxyStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: second\n\n")
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "test", "gsk-abc")

	srv := httptest.NewServer(New(s, map[string]string{"groq": up.URL}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/p/"+c.ID+"/stream", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		close(release)
		t.Fatalf("first chunk did not arrive before the upstream finished: %v", err)
	}
	if string(buf) != "data: first\n\n" {
		close(release)
		t.Fatalf("first chunk = %q", buf)
	}
	close(release)
}

// A class A provider only works when the upstream believes it is talking to the
// genuine tool. Identity headers must therefore survive a caller that sends its
// own, and feature headers must not be overwritten when the caller did send one.
func TestProxyAppliesClassAIdentityAndDefaults(t *testing.T) {
	var gotUA, gotApp, gotVersion, gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotApp = r.Header.Get("X-App")
		gotVersion = r.Header.Get("Anthropic-Version")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.CreateConnection("claude", "test", "oauth-token")
	if err != nil {
		t.Fatal(err)
	}

	h := New(s, map[string]string{"claude": up.URL})
	req := httptest.NewRequest("POST", "/p/"+c.ID+"/messages", strings.NewReader(`{}`))
	// The caller sends its own identity and its own feature selection.
	req.Header.Set("User-Agent", "some-other-client/1.0")
	req.Header.Set("Anthropic-Beta", "caller-chose-this")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotUA != "claude-cli/2.1.278 (external, sdk-cli)" {
		t.Errorf("User-Agent = %q: identity must replace whatever the caller sent", gotUA)
	}
	if gotApp != "cli" {
		t.Errorf("X-App = %q, want cli", gotApp)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q: a missing default must be filled", gotVersion)
	}
	if gotBeta != "caller-chose-this" {
		t.Errorf("Anthropic-Beta = %q: a default must not overwrite the caller's choice", gotBeta)
	}
}

func TestProxyRejectsUnknownConnection(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	req := httptest.NewRequest("POST", "/p/does-not-exist/chat/completions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("the error body mentions the credential")
	}
}

// A connection whose provider is class A must be refused in phase 1 rather than
// reached with a class B code path that cannot impersonate the real tool.
func TestProxyRefusesUnsupportedProvider(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("antigravity", "class A", "token")

	h := New(s, nil)
	req := httptest.NewRequest("POST", "/p/"+c.ID+"/v1internal:generateContent", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a provider this build cannot serve", rec.Code)
	}
}

// Passthrough is not a POST-only idea. A provider exposes GET endpoints too
// (model listings, quota), and a proxy that refuses them is not transparent.
func TestProxyForwardsNonPostMethods(t *testing.T) {
	var gotMethod, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Write([]byte(`{"data":[]}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "test", "gsk-abc")

	h := New(s, map[string]string{"groq": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/p/"+c.ID+"/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: GET must pass through", rec.Code)
	}
	if gotMethod != "GET" || gotPath != "/models" {
		t.Errorf("upstream saw %s %s, want GET /models", gotMethod, gotPath)
	}
}
