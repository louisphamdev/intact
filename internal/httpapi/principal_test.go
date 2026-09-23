package httpapi

import (
	"context"
	"encoding/json"
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

// keyThroughDashboard makes an API key the way an operator does, so the test
// covers the name createKey stores.
func keyThroughDashboard(t *testing.T, h http.Handler, ck *http.Cookie, name string) store.APIKey {
	t.Helper()
	req := loopbackRequest("POST", "/keys", strings.NewReader(`{"name":`+quote(name)+`}`))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var k store.APIKey
	json.Unmarshal(rec.Body.Bytes(), &k)
	if !strings.HasPrefix(k.Key, "sk-intact-") {
		t.Fatalf("create key %q: %d %s", name, rec.Code, rec.Body.String())
	}
	return k
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func getAs(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := loopbackRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getWithCookie(h http.Handler, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := loopbackRequest("GET", path, nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A key's display name is free text. It must never decide admin rights.
func TestAKeyNameCannotGrantAdminRights(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	for _, name := range []string{"laptop", "internal", " internal ", "env"} {
		k := keyThroughDashboard(t, h, ck, name)
		add := rpc(t, h, k.Key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_filter",
			"arguments":{"provider":"groq","kind":"field","pattern":"planted_by_a_key"}}}`)
		if text, isErr := toolText(t, add); !isErr || !strings.Contains(text, "master token") {
			t.Errorf("add_filter as %q: isErr=%v text=%s", name, isErr, text)
		}
		put := rpc(t, h, k.Key, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"put_provider_def",
			"arguments":{"def":{"id":"planted","kind":"apikey","api":"openai","baseUrl":"https://evil.example/v1"}}}}`)
		if text, isErr := toolText(t, put); !isErr || !strings.Contains(text, "master token") {
			t.Errorf("put_provider_def as %q: isErr=%v text=%s", name, isErr, text)
		}
	}
	list := rpc(t, h, cfg.APIToken, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_filters","arguments":{}}}`)
	if text, _ := toolText(t, list); strings.Contains(text, "planted_by_a_key") {
		t.Errorf("a key added a filter: %s", text)
	}
	defs, _ := s.ProviderDefs()
	for _, d := range defs {
		if d.ID == "planted" {
			t.Error("a key declared a provider")
		}
	}
	// The master token still runs the same tool.
	ok := rpc(t, h, cfg.APIToken, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"add_filter",
		"arguments":{"provider":"groq","kind":"field","pattern":"added_by_the_master_token"}}}`)
	if text, isErr := toolText(t, ok); isErr {
		t.Errorf("add_filter with the master token: %s", text)
	}
}

// Two keys can carry the same name, so the stored bodies belong to the key id.
func TestErrorBodiesBelongToTheKeyThatCausedThem(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "secret-prompt-text") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"refused"},"detail":"secret-answer-text"}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	cfg := authConfig()
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, cfg)
	ck := loginCookie(t, h, cfg)

	keyA := keyThroughDashboard(t, h, ck, "key")
	// The same name, put straight into the store: a duplicate can already exist.
	keyB, err := s.CreateAPIKey("key")
	if err != nil {
		t.Fatalf("create key B: %v", err)
	}
	keyC := keyThroughDashboard(t, h, ck, "env")

	if rec := postV1As(h, keyB.Key, `{"model":"groq/llama","messages":[{"role":"user","content":"secret-prompt-text"}]}`); rec.Code != 400 {
		t.Fatalf("key B call: %d %s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListUpstreamErrors(store.ErrorFilter{})
	if len(list) != 1 {
		t.Fatalf("stored errors = %d", len(list))
	}
	id := list[0].ID
	if list[0].Client != "key" {
		t.Errorf("client label = %q, want the key's name", list[0].Client)
	}

	hidden := func(what, body string) {
		t.Helper()
		if strings.Contains(body, "secret-prompt-text") || strings.Contains(body, "secret-answer-text") {
			t.Errorf("%s reads another client's bodies: %s", what, body)
		}
	}
	shown := func(what, body string) {
		t.Helper()
		if !strings.Contains(body, "secret-prompt-text") || !strings.Contains(body, "secret-answer-text") {
			t.Errorf("%s lost the bodies: %s", what, body)
		}
	}
	for _, k := range []struct{ what, tok string }{{"key A", keyA.Key}, {`key named "env"`, keyC.Key}} {
		hidden(k.what, getAs(h, "/api/errors/"+itoa(id), k.tok).Body.String())
		hidden(k.what, getAs(h, "/api/errors", k.tok).Body.String())
	}
	shown("the key that caused it", getAs(h, "/api/errors/"+itoa(id), keyB.Key).Body.String())
	shown("the master token", getAs(h, "/api/errors/"+itoa(id), cfg.APIToken).Body.String())
	shown("the session", getWithCookie(h, "/errors/"+itoa(id), ck).Body.String())

	// A dashboard key still serves its own traffic.
	if rec := postV1As(h, keyA.Key, `{"model":"groq/llama","messages":[{"role":"user","content":"hello"}]}`); rec.Code != 200 {
		t.Errorf("/v1 with a key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := getAs(h, "/api/quota", keyA.Key); rec.Code != 200 {
		t.Errorf("/api/quota with a key: %d %s", rec.Code, rec.Body.String())
	}
}

func postV1As(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := loopbackRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A declared provider carries the credentials of every account that signs in
// through it, so a caller that is not admin reads them masked.
func TestProviderDefCredentialsAreMaskedForAKey(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)
	k := keyThroughDashboard(t, h, ck, "laptop")

	def := `{"id":"acme","kind":"oauth-code","api":"openai","baseUrl":"https://acme.example/v1",
		"headers":{"X-Org-Key":"h-secret"},
		"oauth":{"authorizeUrl":"https://acme.example/auth","tokenUrl":"https://acme.example/token",
		"redirectUri":"https://acme.example/cb","clientId":"cid","clientSecret":"s3cret"}}`
	req := loopbackRequest("POST", "/provider-defs", strings.NewReader(def))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("declare: %d %s", rec.Code, rec.Body.String())
	}
	defer provider.SetDeclared(nil)

	secret := func(what, body string) {
		t.Helper()
		if strings.Contains(body, "s3cret") || strings.Contains(body, "h-secret") {
			t.Errorf("%s leaks a credential: %s", what, body)
		}
	}
	mcpList := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_provider_defs","arguments":{}}}`
	text, _ := toolText(t, rpc(t, h, k.Key, mcpList))
	secret("list_provider_defs with a key", text)
	secret("GET /api/provider-defs with a key", getAs(h, "/api/provider-defs", k.Key).Body.String())
	secret("GET /api/provider-defs/{id} with a key", getAs(h, "/api/provider-defs/acme", k.Key).Body.String())

	// An admin reads the def as stored, so an edit-and-save round trip never
	// writes a mask.
	full, _ := toolText(t, rpc(t, h, cfg.APIToken, mcpList))
	if !strings.Contains(full, "s3cret") || !strings.Contains(full, "h-secret") {
		t.Errorf("list_provider_defs with the master token = %s", full)
	}
	if b := getAs(h, "/api/provider-defs", cfg.APIToken).Body.String(); !strings.Contains(b, "s3cret") || !strings.Contains(b, "h-secret") {
		t.Errorf("GET /api/provider-defs with the master token = %s", b)
	}
	if b := getWithCookie(h, "/provider-defs/acme", ck).Body.String(); !strings.Contains(b, "s3cret") || !strings.Contains(b, "h-secret") {
		t.Errorf("GET /provider-defs/{id} with the session = %s", b)
	}
	// The stored def is untouched by a masked read.
	d, ok := provider.Declared("acme")
	if !ok || d.OAuth.ClientSecret != "s3cret" || d.Headers["X-Org-Key"] != "h-secret" {
		t.Errorf("the stored def changed: %+v", d)
	}
}

// For Slack, Discord, ntfy and n8n the webhook url IS the credential.
func TestWebhookURLNeverReachesADashboardKey(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)
	k := keyThroughDashboard(t, h, ck, "laptop")

	ch := `{"name":"ops","type":"webhook","enabled":true,"config":{"url":"https://hooks.example.com/services/T/B/SECRET"}}`
	req := loopbackRequest("POST", "/notify/channels", strings.NewReader(ch))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create channel: %d %s", rec.Code, rec.Body.String())
	}
	s.NoteNotifySent(channelID(t, s), `Post "https://hooks.example.com/services/T/B/SECRET": dial error`)

	if b := getAs(h, "/api/notify", k.Key).Body.String(); strings.Contains(b, "SECRET") {
		t.Errorf("GET /api/notify with a key leaks the url: %s", b)
	}
	if b := getWithCookie(h, "/notify", ck).Body.String(); !strings.Contains(b, "SECRET") {
		t.Errorf("the session lost the url: %s", b)
	}
	// The MCP tool reads the same channels.
	text, isErr := toolText(t, rpc(t, h, k.Key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_notify_channels","arguments":{}}}`))
	if !isErr || strings.Contains(text, "SECRET") {
		t.Errorf("list_notify_channels with a key: isErr=%v text=%s", isErr, text)
	}
}

func channelID(t *testing.T, s *store.Store) string {
	t.Helper()
	list, _ := s.ListNotifyChannels()
	if len(list) != 1 {
		t.Fatalf("channels = %d", len(list))
	}
	return list[0].ID
}

// A failed send must not put the webhook url into the stored error.
func TestWebhookSendErrorHidesTheURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sendWebhook(ctx, map[string]string{"url": "https://hooks.invalid.example/services/T/B/SECRET"}, notifyMsg{Title: "t"})
	if err == nil {
		t.Fatal("an unreachable webhook returned no error")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/services/") {
		t.Errorf("the error carries the url: %v", err)
	}
}
