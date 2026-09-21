package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestRoundRobinFailsOverOnBusy(t *testing.T) {
	var mu sync.Mutex
	n := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Write([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "gsk-a")
	s.CreateConnection("groq", "b", "gsk-b")
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/groq/chat/completions", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200 after failover; body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Errorf("upstream hits=%d, want 2 (one busy, one served)", n)
	}
}

func TestRoundRobinRotatesAcrossCalls(t *testing.T) {
	var mu sync.Mutex
	auths := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "gsk-a")
	s.CreateConnection("groq", "b", "gsk-b")
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, nil)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/groq/x", strings.NewReader("{}")))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d code=%d", i, rec.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Errorf("two calls used auths %v, want two different accounts", auths)
	}
}

func TestRoundRobinNoActiveAccount(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := NewWithAuth(s, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/r/groq/x", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("no account: code=%d, want 404", rec.Code)
	}
}
