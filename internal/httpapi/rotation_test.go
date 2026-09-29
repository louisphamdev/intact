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

// keysServer answers every call and records which key made each chat call
// (not the quota reads that follow a 429); keys in busy are refused with 429.
func keysServer(busy map[string]bool) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method != http.MethodPost {
			w.Write([]byte(`{}`))
			return
		}
		mu.Lock()
		seen = append(seen, k)
		mu.Unlock()
		if busy[k] {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	return up, func() []string { mu.Lock(); defer mu.Unlock(); out := seen; seen = nil; return out }
}

func TestRotationStickyFallbackAndOrder(t *testing.T) {
	busy := map[string]bool{}
	up, seen := keysServer(busy)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, _ := s.CreateConnection("groq", "a", "ka")
	b, _ := s.CreateConnection("groq", "b", "kb")
	c, _ := s.CreateConnection("groq", "c", "kc")
	h := New(s, map[string]string{"groq": up.URL})
	call := func(n int) string {
		for i := 0; i < n; i++ {
			postV1(h, `{"model":"groq/m","messages":[]}`)
		}
		return strings.Join(seen(), " ")
	}
	set := func(body string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/providers/groq/rotation", strings.NewReader(body)))
		return rec.Body.String()
	}
	// Default: round-robin, a turn per request.
	if got := call(4); got != "ka kb kc ka" {
		t.Errorf("round-robin = %q", got)
	}
	// Two requests per turn, from the first account again.
	set(`{"sticky":2}`)
	if got := call(6); got != "ka ka kb kb kc kc" {
		t.Errorf("sticky 2 = %q", got)
	}
	// Priority order c, a, b.
	set(`{"order":["` + c.ID + `","` + a.ID + `"]}`)
	if got := call(4); got != "kc kc ka ka" {
		t.Errorf("ordered = %q", got)
	}
	// Fallback: always the first, the next only when it is busy.
	if got := set(`{"mode":"fallback"}`); !strings.Contains(got, `"next":"`+c.ID+`"`) {
		t.Errorf("state = %s", got)
	}
	if got := call(3); got != "kc kc kc" {
		t.Errorf("fallback = %q", got)
	}
	busy["kc"] = true
	if got := call(1); got != "kc ka" {
		t.Errorf("fallback when busy = %q", got)
	}
	_ = b
	if got := set(`{"sticky":0}`); !strings.Contains(got, "sticky is 1 to 1000") {
		t.Errorf("sticky 0 accepted: %s", got)
	}
}

func TestStandbyAccountServesOnlyWhenEveryPrimaryFails(t *testing.T) {
	busy := map[string]bool{}
	up, seen := keysServer(busy)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	sb, _ := s.CreateConnection("groq", "s", "ks")
	s.CreateConnection("groq", "b", "kb")
	h := New(s, map[string]string{"groq": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/accounts/"+sb.ID+"/active", strings.NewReader(`{"active":true,"standby":true}`)))
	if rec.Code != 200 {
		t.Fatalf("set standby: %d %s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListConnections()
	for _, c := range list {
		if c.Standby != (c.ID == sb.ID) || !c.IsActive {
			t.Errorf("stored: %+v", c)
		}
	}
	call := func(n int) string {
		for i := 0; i < n; i++ {
			postV1(h, `{"model":"groq/m","messages":[]}`)
		}
		return strings.Join(seen(), " ")
	}
	state := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("GET", "/providers/groq/rotation", nil))
		return rec.Body.String()
	}
	// The primaries take turns; the standby is never one of them.
	if got := call(4); strings.Contains(got, "ks") || strings.Count(got, "ka") != 2 || strings.Count(got, "kb") != 2 {
		t.Errorf("round-robin = %q", got)
	}
	for i := 0; i < 3; i++ {
		if st := state(); strings.Contains(st, `"next":"`+sb.ID+`"`) {
			t.Errorf("the standby is shown as next: %s", st)
		}
		call(1)
	}
	// One primary busy: the other one answers, not the standby.
	busy["ka"] = true
	if got := call(2); strings.Contains(got, "ks") {
		t.Errorf("one primary busy = %q", got)
	}
	// Every primary busy: the standby answers last.
	busy["kb"] = true
	if got := call(1); !strings.HasSuffix(got, "ks") || strings.Count(got, "ks") != 1 {
		t.Errorf("all primaries busy = %q", got)
	}
	// Fallback mode keeps the standby after every primary too.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/providers/groq/rotation", strings.NewReader(`{"mode":"fallback","order":["`+sb.ID+`"]}`)))
	delete(busy, "ka")
	delete(busy, "kb")
	if got := call(2); strings.Contains(got, "ks") {
		t.Errorf("fallback with the standby first in order = %q", got)
	}
	// Back to a normal account: it takes turns again.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/accounts/"+sb.ID+"/active", strings.NewReader(`{"active":true,"standby":false}`)))
	if got := call(1); got != "ks" {
		t.Errorf("standby off, first in fallback order = %q", got)
	}
	// Switching an account off clears its standby mark: off means never.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/accounts/"+sb.ID+"/active", strings.NewReader(`{"active":false}`)))
	busy["ka"], busy["kb"] = true, true
	if got := call(1); strings.Contains(got, "ks") {
		t.Errorf("an account that is off was called: %q", got)
	}
}
