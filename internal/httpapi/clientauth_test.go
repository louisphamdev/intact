package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A provider's own client is let in without a token, but only on the proxy routes, only for its
// own provider, and it gets no right it did not have as a caller. Each of those is a separate
// thing that could be got wrong, so each is a separate test.

func clientAuthAPI(t *testing.T) http.Handler {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Auth on: with auth off every caller is admin and the gate is not there to test.
	return NewWithAuth(s, nil, authConfig())
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

// Both coding tools' own clients are recognised, because both of them carry a credential from
// their provider rather than one intact issued.
func TestEachProvidersOwnClientIsRecognised(t *testing.T) {
	h := clientAuthAPI(t)
	for _, ua := range []string{"codex_cli_rs/0.160.1", "claude-cli/2.1.292", "antigravity/cli/1.2.3"} {
		w := call(h, "GET", "/v1/models", ua, "not-an-intact-token", "")
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%q was refused: the exemption is not reaching every provider", ua)
		}
	}
}

// The door the whole thing exists for: Codex sends ChatGPT's own credential, which intact cannot
// check. It has to get through to reach the codex provider.
func TestProvidersOwnClientReachesTheProxyWithoutAToken(t *testing.T) {
	h := clientAuthAPI(t)
	w := call(h, "GET", "/v1/models", "codex_cli_rs/0.160.1", "a-chatgpt-token-intact-never-issued", "")
	// No account serves anything in this fixture, so the answer is not 401; it is the routing
	// answer. What matters is that the gate let it through at all.
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("the provider's own client was refused at the door: %s", w.Body.String())
	}
}

func TestACallerWithNoTokenAndNoClientNameIsStillRefused(t *testing.T) {
	h := clientAuthAPI(t)
	for _, ua := range []string{"", "curl/8", "python-requests/2", "my-proxy/1 claude", "xclaude-cli/2"} {
		w := call(h, "GET", "/v1/models", ua, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("User-Agent %q got %d, want 401", ua, w.Code)
		}
	}
}

// The User-Agent must not become a costume. This is the finding that matters most: the
// exemption is scoped to the provider that declared the prefix.
func TestForgedClientNameReachesOnlyItsOwnProvider(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("claude", "acct", "secret")
	s.CreateConnection("codex", "acct", "secret")
	h := NewWithAuth(s, map[string]string{"claude": "http://127.0.0.1:1", "codex": "http://127.0.0.1:1"}, authConfig())

	// The codex client asking for a claude model: refused, even though the name is right.
	body := `{"model":"claude/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	w := call(h, "POST", "/v1/messages", "codex_cli_rs/0.160.1", "", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("the codex client reached the claude provider: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "codex") {
		t.Errorf("the refusal does not say why: %s", w.Body.String())
	}
}

// The exemption is on the proxy only. A forged name must not open the dashboard's reads.
func TestForgedClientNameOpensNoApiRoute(t *testing.T) {
	h := clientAuthAPI(t)
	for _, path := range []string{"/api/usage", "/api/quota", "/api/errors", "/api/drift/changes", "/api/providers"} {
		w := call(h, "GET", path, "codex_cli_rs/0.160.1", "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with a forged client name got %d, want 401", path, w.Code)
		}
	}
}

func TestProviderClientForMatchesTheDeclaredPrefixOnly(t *testing.T) {
	cases := []struct {
		ua  string
		id  string
		yes bool
	}{
		{"codex_cli_rs/0.160.1", "codex", true},
		// `codex exec` is the same client in a different binary and says so.
		{"codex_exec/0.160.1 (Debian 13.0.0; x86_64)", "codex", true},
		{"claude-cli/2.1.292", "claude", true},
		{"antigravity/cli/1.2", "antigravity", true},
		{"codex-cli/0.160.1", "", false},
		// The declared prefix carries a slash, so a bare name is not the client's. Refusing
		// it is the safe side: only a real client spells it with a version.
		{"codex_cli_rs", "", false},
		{"codex_cli_rs_extra/1", "", false},
		{" notcodex_cli_rs/1", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		id, ok := providerClientFor(c.ua)
		if ok != c.yes || id != c.id {
			t.Errorf("providerClientFor(%q) = %q,%v want %q,%v", c.ua, id, ok, c.id, c.yes)
		}
	}
}

// A client that came in without a token owns nothing, whatever it asks for.
func TestProviderClientPrincipalHoldsNoRight(t *testing.T) {
	p := principal{name: "client:codex", providerClient: "codex"}
	if p.admin {
		t.Error("a provider's own client must not be admin")
	}
	if p.keyID != "" {
		t.Error("a provider's own client must own no key")
	}
}
