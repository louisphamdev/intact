package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// fakeProvider serves a model list and records the auth and body of each call.
type fakeProvider struct {
	mu     sync.Mutex
	models string
	calls  []string // "auth|body"
	busy   func(auth string) bool
}

func (f *fakeProvider) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(f.models))
			return
		}
		b, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		f.calls = append(f.calls, auth+"|"+string(b))
		busy := f.busy != nil && f.busy(auth)
		f.mu.Unlock()
		if busy {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func postV1(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func TestV1PrefixRotatesAccountsAndStripsPrefix(t *testing.T) {
	f := &fakeProvider{models: `{"data":[]}`}
	url := f.start(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": url})

	for i := 0; i < 2; i++ {
		if rec := postV1(h, `{"model":"groq/llama","stream":false}`); rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if len(f.calls) != 2 || f.calls[0] == f.calls[1] {
		t.Fatalf("calls = %q, want two different accounts", f.calls)
	}
	for _, c := range f.calls {
		if !strings.HasSuffix(c, `|{"model":"llama","stream":false}`) {
			t.Errorf("upstream body = %q, want the prefix stripped and the rest unchanged", c)
		}
	}
}

func TestV1BareModelPoolsEveryProviderThatListsIt(t *testing.T) {
	groq := &fakeProvider{models: `{"data":[{"id":"llama"}]}`, busy: func(string) bool { return true }}
	nv := &fakeProvider{models: `{"data":[{"id":"llama"},{"id":"other"}]}`}
	or := &fakeProvider{models: `{"data":[{"id":"qwen"}]}`}
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "kg")
	s.CreateConnection("nvidia", "n", "kn")
	s.CreateConnection("openrouter", "o", "ko")
	h := New(s, map[string]string{"groq": groq.start(t), "nvidia": nv.start(t), "openrouter": or.start(t)})

	// groq is busy, so the pool fails over to nvidia; openrouter does not list
	// the model and is never tried. The body goes out as sent.
	rec := postV1(h, `{"model":"llama"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(nv.calls) != 1 || nv.calls[0] != `Bearer kn|{"model":"llama"}` {
		t.Errorf("nvidia calls = %q", nv.calls)
	}
	if len(or.calls) != 0 {
		t.Errorf("openrouter was tried for a model it does not list: %q", or.calls)
	}
}

func TestV1RejectsMissingOrUnknownModel(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"llama"}]}`}
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": f.start(t)})

	if rec := postV1(h, `{"messages":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no model: code=%d, want 400", rec.Code)
	}
	if rec := postV1(h, `{"model":"not-served"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown model: code=%d, want 404", rec.Code)
	}
	if len(f.calls) != 0 {
		t.Errorf("upstream called for a request that could not route: %q", f.calls)
	}
}

func TestV1CustomProviderUsesItsBaseURL(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("tokenharbor", "TH", "th-key")
	s.SetBaseURL(c.ID, up.URL+"/v1")
	h := New(s, nil)

	if rec := postV1(h, `{"model":"tokenharbor/gpt-x"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer th-key" || gotBody != `{"model":"gpt-x"}` {
		t.Errorf("upstream got path=%q auth=%q body=%q", gotPath, gotAuth, gotBody)
	}
}

func TestCreateAccountFillsCloudflareAccountID(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	form := "provider=cloudflare-ai&label=cf&secret=k&account_id=abc123"
	req := httptest.NewRequest("POST", "/accounts", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListConnections()
	want := "https://api.cloudflare.com/client/v4/accounts/abc123/ai/v1"
	if len(list) != 1 || list[0].BaseURL != want {
		t.Fatalf("connections = %+v, want base %s", list, want)
	}

	// A custom provider without a base URL is refused.
	req = httptest.NewRequest("POST", "/accounts", strings.NewReader("provider=mine&secret=k"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("custom provider without base url: code=%d", rec.Code)
	}
}
