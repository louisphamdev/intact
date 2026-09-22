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

// recorder is an upstream that logs the auth and body of each call and answers
// with the status its busy function picks.
type recorder struct {
	mu    sync.Mutex
	calls []string // "auth|body"
	busy  func(auth string) bool
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		rc.mu.Lock()
		rc.calls = append(rc.calls, auth+"|"+string(b))
		rc.mu.Unlock()
		if rc.busy != nil && rc.busy(auth) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

func TestGroupSpansProvidersAndRewritesModel(t *testing.T) {
	rc := &recorder{busy: func(auth string) bool { return auth == "Bearer gsk" }}
	up := rc.server(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	g1, _ := s.CreateConnection("groq", "g", "gsk")
	n1, _ := s.CreateConnection("nvidia", "n", "nvk")
	s.SaveGroup(store.Group{Name: "llama", Strategy: store.StrategyFallback, Members: []store.Member{
		{ConnectionID: g1.ID}, {ConnectionID: n1.ID, Model: "meta/llama-3.3-70b-instruct"},
	}})
	h := New(s, map[string]string{"groq": up.URL, "nvidia": up.URL})

	rec := post(h, "/g/llama/chat/completions", `{"model":"llama-3.3-70b-versatile","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	want := []string{
		`Bearer gsk|{"model":"llama-3.3-70b-versatile","stream":false}`,
		`Bearer nvk|{"model":"meta/llama-3.3-70b-instruct","stream":false}`,
	}
	if len(rc.calls) != 2 || rc.calls[0] != want[0] || rc.calls[1] != want[1] {
		t.Fatalf("calls = %q, want %q", rc.calls, want)
	}
}

func TestGroupRoundRobinRotatesAndSkipsInactive(t *testing.T) {
	rc := &recorder{}
	up := rc.server(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, _ := s.CreateConnection("groq", "a", "ka")
	b, _ := s.CreateConnection("openrouter", "b", "kb")
	c, _ := s.CreateConnection("nvidia", "c", "kc")
	s.SetActive(c.ID, false)
	s.SaveGroup(store.Group{Name: "mix", Strategy: store.StrategyRoundRobin, Members: []store.Member{
		{ConnectionID: a.ID}, {ConnectionID: b.ID}, {ConnectionID: c.ID},
	}})
	h := New(s, map[string]string{"groq": up.URL, "openrouter": up.URL, "nvidia": up.URL})

	for i := 0; i < 4; i++ {
		if rec := post(h, "/g/mix/x", `{}`); rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d", i, rec.Code)
		}
	}
	got := []string{}
	for _, c := range rc.calls {
		got = append(got, strings.SplitN(c, "|", 2)[0])
	}
	want := "Bearer ka,Bearer kb,Bearer ka,Bearer kb"
	if strings.Join(got, ",") != want {
		t.Fatalf("auths = %v, want %s (inactive member skipped)", got, want)
	}
}

func TestGroupUnknownOrEmpty(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.SaveGroup(store.Group{Name: "empty", Strategy: store.StrategyRoundRobin})
	h := New(s, nil)
	if rec := post(h, "/g/nope/x", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown group code=%d", rec.Code)
	}
	if rec := post(h, "/g/empty/x", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("empty group code=%d", rec.Code)
	}
}

func TestGroupEndpointsNeedSessionAndSave(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "a", "k")
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	body := `{"name":"fast","strategy":"fallback","members":[{"connectionId":"` + c.ID + `"}]}`
	if rec := post(h, "/groups", body); rec.Code != http.StatusFound {
		t.Fatalf("no session: code=%d, want redirect to login", rec.Code)
	}
	ck := loginCookie(t, h, cfg)
	req := httptest.NewRequest("POST", "/groups", strings.NewReader(body))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"strategy":"fallback"`) {
		t.Fatalf("save code=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", "/groups", strings.NewReader(`{"name":"Bad Name"}`))
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad name code=%d", rec.Code)
	}
}

func TestGroupProviderMemberRotatesAccounts(t *testing.T) {
	rc := &recorder{}
	up := rc.server(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	s.CreateConnection("groq", "b", "kb")
	s.CreateConnection("nvidia", "n", "kn")
	s.SaveGroup(store.Group{Name: "llama", Strategy: store.StrategyFallback, Members: []store.Member{
		{Provider: "groq"}, {Provider: "nvidia", Model: "meta/llama"},
	}})
	h := New(s, map[string]string{"groq": up.URL, "nvidia": up.URL})
	for i := 0; i < 2; i++ {
		if rec := post(h, "/g/llama/x", `{"model":"llama"}`); rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d", i, rec.Code)
		}
	}
	// Fallback keeps groq first; the groq member itself rotates its two keys.
	if len(rc.calls) != 2 || rc.calls[0] == rc.calls[1] ||
		!strings.HasPrefix(rc.calls[0], "Bearer k") || strings.Contains(rc.calls[0]+rc.calls[1], "kn") {
		t.Fatalf("calls = %q, want two different groq keys", rc.calls)
	}

	// With every groq key busy, the run falls through to nvidia with its model.
	rc.busy = func(auth string) bool { return auth != "Bearer kn" }
	rc.calls = nil
	if rec := post(h, "/g/llama/x", `{"model":"llama"}`); rec.Code != http.StatusOK {
		t.Fatalf("failover code=%d", rec.Code)
	}
	last := rc.calls[len(rc.calls)-1]
	if len(rc.calls) != 3 || last != `Bearer kn|{"model":"meta/llama"}` {
		t.Fatalf("calls = %q, want both groq keys then nvidia", rc.calls)
	}
}
