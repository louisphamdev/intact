package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

func clientAuthAPI(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, NewWithAuth(s, nil, authConfig())
}

func call(h http.Handler, method, path, ua, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:20130"
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func callWithHeader(h http.Handler, method, path, ua, headerKey, headerVal, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:20130"
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	if headerKey != "" {
		r.Header.Set(headerKey, headerVal)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Gate decision is credential-only: tokenPrincipal accepts a verified credential or the gate returns 401.
// A spoofed User-Agent with no credential or an invalid/expired credential must never bypass authentication,
// and zero upstream requests must reach the provider.
func TestForgedUserAgentWithoutValidCredentialRefused401(t *testing.T) {
	upstreamCalled := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","id":"msg_ok","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateConnection("claude", "acct", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConnection("codex", "acct", "stored-secret"); err != nil {
		t.Fatal(err)
	}

	h := NewWithAuth(s, map[string]string{"claude": up.URL, "codex": up.URL}, authConfig())

	// Build forged UA list from registered Bifrost providers
	var forgedUAs []string
	for _, id := range provider.BifrostProviderIDs() {
		p, ok := provider.Lookup(id)
		if ok && p.BifrostUA != "" {
			for _, prefix := range strings.Split(p.BifrostUA, ",") {
				prefix = strings.TrimSpace(prefix)
				if prefix != "" {
					forgedUAs = append(forgedUAs, prefix+"1.0.0", prefix+"2.1.292 (external, cli)", prefix)
				}
			}
		}
	}
	forgedUAs = append(forgedUAs, "codex_cli_rs/0.160.1", "claude-cli/2.1.293", "antigravity/cli/1.2.3", "curl/8", "")

	body := `{"model":"claude/m","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`

	for _, ua := range forgedUAs {
		// 1. Without token
		w := call(h, "POST", "/v1/messages", ua, "", body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("UA %q with no token got %d, want 401", ua, w.Code)
		}
		// 2. With invalid token
		w = call(h, "POST", "/v1/messages", ua, "invalid-forged-token", body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("UA %q with invalid token got %d, want 401", ua, w.Code)
		}
	}

	// 3. With expired key
	expiredKey, err := s.CreateAPIKey("expired key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAPIKeyLimits(expiredKey.ID, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), 0); err != nil {
		t.Fatal(err)
	}
	w := call(h, "POST", "/v1/messages", "claude-cli/2.1.293", expiredKey.Key, body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired key got %d, want 401", w.Code)
	}

	if upstreamCalled != 0 {
		t.Fatalf("upstream was called %d times on unauthenticated requests; want 0", upstreamCalled)
	}
}

// Parity test: An intact-issued credential admits requests with or without a Bifrost User-Agent,
// producing identical status, body, and upstream call count.
func TestAuthenticatedRequestsSucceedWithOrWithoutUA(t *testing.T) {
	var recordedAuth []string
	var recordedBodies [][]byte

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authVal := r.Header.Get("x-api-key")
		if authVal == "" {
			authVal = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		recordedAuth = append(recordedAuth, authVal)
		b, _ := io.ReadAll(r.Body)
		recordedBodies = append(recordedBodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":2}}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.CreateConnection("claude", "acct", "stored-cred-123"); err != nil {
		t.Fatal(err)
	}
	apiKey, err := s.CreateAPIKey("valid key", nil)
	if err != nil {
		t.Fatal(err)
	}

	cfg := authConfig()
	h := NewWithAuth(s, map[string]string{"claude": up.URL}, cfg)

	body := `{"model":"claude/m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`

	// 1. With intact API key and Bifrost UA
	wWithUA := call(h, "POST", "/v1/messages", "claude-cli/2.1.292", apiKey.Key, body)
	// 2. With intact API key and generic UA
	wWithoutUA := call(h, "POST", "/v1/messages", "generic-client/1.0", apiKey.Key, body)

	if wWithUA.Code != http.StatusOK {
		t.Fatalf("with UA got %d: %s", wWithUA.Code, wWithUA.Body.String())
	}
	if wWithoutUA.Code != http.StatusOK {
		t.Fatalf("without UA got %d: %s", wWithoutUA.Code, wWithoutUA.Body.String())
	}
	if wWithUA.Body.String() != wWithoutUA.Body.String() {
		t.Fatalf("parity mismatch between UA and non-UA responses:\nwith UA: %s\nwithout UA: %s",
			wWithUA.Body.String(), wWithoutUA.Body.String())
	}

	// 3. Test X-Api-Key header format
	wXApiKey := callWithHeader(h, "POST", "/v1/messages", "claude-cli/2.1.292", "x-api-key", apiKey.Key, body)
	if wXApiKey.Code != http.StatusOK {
		t.Fatalf("x-api-key header got %d: %s", wXApiKey.Code, wXApiKey.Body.String())
	}

	// 4. Test master token ("machine-tok")
	wMaster := call(h, "POST", "/v1/messages", "claude-cli/2.1.292", "machine-tok", body)
	if wMaster.Code != http.StatusOK {
		t.Fatalf("master token got %d: %s", wMaster.Code, wMaster.Body.String())
	}

	// 5. Test dashboard session cookie
	rCookie := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rCookie.Header.Set("Content-Type", "application/json")
	rCookie.AddCookie(&http.Cookie{Name: sessionCookie, Value: cfg.IssueSession()})
	wCookie := httptest.NewRecorder()
	h.ServeHTTP(wCookie, rCookie)
	if wCookie.Code != http.StatusOK {
		t.Fatalf("session cookie got %d: %s", wCookie.Code, wCookie.Body.String())
	}

	// Check that upstream received the stored credential, not the caller's intact key
	for _, authVal := range recordedAuth {
		if authVal != "stored-cred-123" {
			t.Errorf("upstream received %q, want stored-cred-123", authVal)
		}
	}
}

// Model permissions: key scoped to model set S returns 403 when asking for a model outside S,
// with zero upstream requests.
func TestModelPermissionsRefuseUnauthorizedModel(t *testing.T) {
	upstreamCalled := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","id":"msg_ok","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateConnection("claude", "acct", "secret"); err != nil {
		t.Fatal(err)
	}

	// Key permitted ONLY for claude/allowed-model
	key, err := s.CreateAPIKey("scoped key", []string{"claude/allowed-model"})
	if err != nil {
		t.Fatal(err)
	}

	h := NewWithAuth(s, map[string]string{"claude": up.URL}, authConfig())

	// Disallowed model
	bodyDisallowed := `{"model":"claude/disallowed-model","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
	w := call(h, "POST", "/v1/messages", "claude-cli/2.1.292", key.Key, bodyDisallowed)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed model got %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model is not permitted for this api key") {
		t.Errorf("unexpected refusal body: %s", w.Body.String())
	}
	if upstreamCalled != 0 {
		t.Fatalf("upstream called %d times on forbidden model, want 0", upstreamCalled)
	}

	// Allowed model
	bodyAllowed := `{"model":"claude/allowed-model","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
	w = call(h, "POST", "/v1/messages", "claude-cli/2.1.292", key.Key, bodyAllowed)
	if w.Code != http.StatusOK {
		t.Fatalf("allowed model got %d: %s", w.Code, w.Body.String())
	}
	if upstreamCalled != 1 {
		t.Fatalf("upstream called %d times on allowed model, want 1", upstreamCalled)
	}
}

// The proxy routes require credentials, and so do /api routes. A forged User-Agent opens no /api route.
func TestForgedClientNameOpensNoApiRoute(t *testing.T) {
	_, h := clientAuthAPI(t)
	for _, path := range []string{"/api/usage", "/api/quota", "/api/errors", "/api/drift/changes", "/api/providers"} {
		w := call(h, "GET", path, "codex_cli_rs/0.160.1", "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with a forged client name got %d, want 401", path, w.Code)
		}
	}
}

// A dashboard key made for /v1 cannot open /api administrative routes requiring admin privileges.
func TestDashboardKeyCannotAccessAdminApiRoutes(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key, err := s.CreateAPIKey("v1 client key", nil)
	if err != nil {
		t.Fatal(err)
	}

	h := NewWithAuth(s, nil, authConfig())
	for _, path := range []string{"/api/accounts/1/active", "/api/filters", "/api/drift/ack"} {
		w := call(h, "POST", path, "claude-cli/2.1.292", key.Key, "{}")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key got %d, want 403", path, w.Code)
		}
	}
}
