package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// These two run the whole path a request takes: a real store, real accounts, a real 429 from the
// provider and the retry that follows. The unit tests above check the parts; these check that the
// parts are wired to each other, which is where a 429 fix would otherwise silently do nothing.

// rateLimitServer answers 429 once and normally after, and records the size of every request it
// receives.
func rateLimitServer(t *testing.T, firstStatus int) (*httptest.Server, *[]int) {
	t.Helper()
	var sizes []int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a routed request is counted. A 429 makes intact read the account's quota,
		// and that read goes to the same upstream on a different path; it is not a retry
		// and its size says nothing about the conversation.
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Write([]byte(`{}`))
			return
		}
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		sizes = append(sizes, len(body))
		if len(sizes) == 1 {
			w.WriteHeader(firstStatus)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(up.Close)
	return up, &sizes
}

// twoAccountAPI is a gateway with two round-robin accounts on one provider, which is the shape
// the feature requires: somewhere for a shortened conversation to go.
func twoAccountAPI(t *testing.T, up *httptest.Server) http.Handler {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("claude", "a", "acct-a")
	s.CreateConnection("claude", "b", "acct-b")
	_, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	if err := s.SetSetting(rotationKey("claude"), `{"mode":"round-robin"}`); err != nil {
		t.Fatal(err)
	}
	return h
}

// roundRobinAPI is oneAccountAPI's counterpart for the tests that set other settings.
func roundRobinAPI(t *testing.T, up *httptest.Server, accounts ...string) http.Handler {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, name := range accounts {
		s.CreateConnection("claude", name, "acct-"+name)
	}
	_, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	if err := s.SetSetting(rotationKey("claude"), `{"mode":"round-robin"}`); err != nil {
		t.Fatal(err)
	}
	return h
}

func agentConversation(turns, resultLen int) []byte {
	msgs := []map[string]any{{"role": "user", "content": "fix the failing test in update.test.mjs"}}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("step %d", i)},
				map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id,
					"content": fmt.Sprintf("out%d:", i) + strings.Repeat("z", resultLen)}}})
	}
	body, _ := json.Marshal(map[string]any{"model": "claude/m", "max_tokens": 1024, "messages": msgs})
	return body
}

func Test429RetryCarriesAShorterConversation(t *testing.T) {
	up, sizes := rateLimitServer(t, http.StatusTooManyRequests)
	h := twoAccountAPI(t, up)
	body := agentConversation(10, 3000)
	t.Logf("client sent %d bytes", len(body))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackRequest("POST", "/v1/messages", strings.NewReader(string(body))))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(*sizes) != 2 {
		t.Fatalf("the provider saw %d requests, want the 429 and one retry", len(*sizes))
	}
	t.Logf("provider saw %d bytes, then %d", (*sizes)[0], (*sizes)[1])
	if (*sizes)[1] >= (*sizes)[0] {
		t.Fatalf("the retry was not shorter: %d then %d", (*sizes)[0], (*sizes)[1])
	}
	saved := (*sizes)[0] - (*sizes)[1]
	t.Logf("the retry carried %d fewer bytes (%.0f%%)", saved, 100*float64(saved)/float64((*sizes)[0]))
}

// A standby account is the last resort, not a ready receiver: it is tried after every other
// account, and only when they all fail. Handing it a shortened conversation would throw history
// away for an account that is still waiting its turn.
func Test429WithOnlyAStandbyKeepsTheFullConversation(t *testing.T) {
	up, sizes := rateLimitServer(t, http.StatusTooManyRequests)
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	first, err := s.CreateConnection("claude", "a", "acct-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateConnection("claude", "b", "acct-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStandby(second.ID, true); err != nil {
		t.Fatal(err)
	}
	_, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	if err := s.SetSetting(rotationKey("claude"), `{"mode":"round-robin"}`); err != nil {
		t.Fatal(err)
	}
	body := agentConversation(10, 3000)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackRequest("POST", "/v1/messages", strings.NewReader(string(body))))
	t.Logf("primary %s, standby %s; provider saw %d requests, sizes %v", first.ID, second.ID, len(*sizes), *sizes)
	for i, n := range *sizes {
		if n != (*sizes)[0] {
			t.Fatalf("request %d was %d bytes, want %d: a standby is not a ready receiver", i, n, (*sizes)[0])
		}
	}
}

// Without a second account there is nowhere to send a shortened conversation.
func Test429WithOneAccountKeepsTheFullConversation(t *testing.T) {
	up, sizes := rateLimitServer(t, http.StatusTooManyRequests)
	h := roundRobinAPI(t, up, "only")
	body := agentConversation(10, 3000)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackRequest("POST", "/v1/messages", strings.NewReader(string(body))))
	t.Logf("provider saw %d requests, sizes %v", len(*sizes), *sizes)
	// One account that says 429 leaves nothing to retry with: the 429 marks its quota spent,
	// so the request ends there. Nothing was shortened, which is the point -- with nowhere to
	// send a shorter conversation, the only way to shorten one would be to throw history away
	// and still have nowhere to put it.
	if len(*sizes) != 1 {
		t.Fatalf("the provider saw %d requests, want only the one that was rate limited", len(*sizes))
	}
}

// A 500 is retried too, and a shorter conversation would not help it: the provider is broken,
// not full. Only 429 compacts.
func TestServerErrorRetryKeepsTheFullConversation(t *testing.T) {
	up, sizes := rateLimitServer(t, http.StatusInternalServerError)
	h := roundRobinAPI(t, up, "a", "b")
	body := agentConversation(10, 3000)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackRequest("POST", "/v1/messages", strings.NewReader(string(body))))
	if len(*sizes) < 2 {
		t.Fatalf("the provider saw %d requests, want a retry", len(*sizes))
	}
	if (*sizes)[1] != (*sizes)[0] {
		t.Fatalf("a 500 retry was shortened from %d to %d bytes: the provider is broken, not full", (*sizes)[0], (*sizes)[1])
	}
}
