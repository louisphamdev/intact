package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// affinityServer records which account answered each request.
func affinityServer(t *testing.T, fail map[string]int) (*httptest.Server, *[]string) {
	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Write([]byte(`{}`)) // quota reads after a 429 are not routed requests
			return
		}
		hits = append(hits, auth)
		if code := fail[auth]; code != 0 {
			w.WriteHeader(code)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(up.Close)
	return up, &hits
}

func affinityAPI(t *testing.T, up *httptest.Server) http.Handler {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("claude", "a", "acct-a")
	s.CreateConnection("claude", "b", "acct-b")
	s.CreateConnection("claude", "c", "acct-c")
	return New(s, map[string]string{"claude": up.URL})
}

func sessionRequest(header, userID string) *http.Request {
	body := `{"model":"claude/m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	if userID != "" {
		body = `{"model":"claude/m","max_tokens":5,"metadata":{"user_id":` + userID + `},"messages":[{"role":"user","content":"hi"}]}`
	}
	r := loopbackRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	if header != "" {
		r.Header.Set("X-Claude-Code-Session-Id", header)
	}
	return r
}

// Anthropic caches per organization, so one session must stay on one account
// for its prompt cache to hit. Round-robin still spreads new sessions.
func TestSessionStaysOnOneAccount(t *testing.T) {
	up, hits := affinityServer(t, nil)
	h := affinityAPI(t, up)
	for i := 0; i < 4; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-1", ""))
	}
	first := (*hits)[0]
	for i, got := range *hits {
		if got != first {
			t.Fatalf("request %d of one session went to %s, first went to %s: %v", i, got, first, *hits)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-2", ""))
	if (*hits)[4] == first {
		t.Errorf("a new session went to %s again: new sessions must still rotate", first)
	}
}

// Claude Code also names the session inside metadata.user_id, a JSON string.
func TestSessionFromMetadataUserID(t *testing.T) {
	up, hits := affinityServer(t, nil)
	h := affinityAPI(t, up)
	uid := `"{\"device_id\":\"d1\",\"account_uuid\":\"u1\",\"session_id\":\"s-meta\"}"`
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("", uid))
	}
	if (*hits)[0] != (*hits)[1] || (*hits)[1] != (*hits)[2] {
		t.Errorf("one metadata session spread over %v", *hits)
	}
}

// When the session's account is limited, the session moves to the account that
// answered and stays there.
func TestSessionMovesWhenItsAccountFails(t *testing.T) {
	fail := map[string]int{}
	up, hits := affinityServer(t, fail)
	h := affinityAPI(t, up)
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-x", ""))
	home := (*hits)[0]
	fail[home] = http.StatusTooManyRequests
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sessionRequest("sess-x", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d after its account was limited", rec.Code)
	}
	moved := (*hits)[len(*hits)-1]
	delete(fail, home)
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-x", ""))
	if got := (*hits)[len(*hits)-1]; got != moved {
		t.Errorf("after the move the session went to %s, want %s (the account that answered); hits %v", got, moved, *hits)
	}
}

// stubClaudeQuota makes the quota reader answer per account label.
func stubClaudeQuota(t *testing.T, win func(label string) QuotaWindow) {
	prev := quotaFetchers["claude"]
	quotaFetchers["claude"] = func(_ *api, _ context.Context, c store.Connection, _ string) (AccountQuota, error) {
		return AccountQuota{ConnectionID: c.ID, Provider: "claude", Windows: []QuotaWindow{win(c.Label)}}, nil
	}
	t.Cleanup(func() { quotaFetchers["claude"] = prev })
}

func loadQuota(t *testing.T, h http.Handler) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/api/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/quota: %d %s", rec.Code, rec.Body.String())
	}
}

// An account whose quota is spent is skipped until its window resets, then it
// takes turns again with no one switching it back on.
func TestSpentAccountIsSkippedUntilItsReset(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Second)
	stubClaudeQuota(t, func(label string) QuotaWindow {
		if label == "a" {
			return QuotaWindow{Name: "5h", UsedPct: 100, ResetAt: resetAt.UTC().Format(time.RFC3339)}
		}
		return QuotaWindow{Name: "5h", UsedPct: 10}
	})
	up, hits := affinityServer(t, nil)
	h := affinityAPI(t, up)
	loadQuota(t, h)
	for i := 0; i < 6; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("", ""))
	}
	for _, got := range *hits {
		if got == "acct-a" {
			t.Fatalf("a spent account was called: %v", *hits)
		}
	}
	time.Sleep(time.Until(resetAt) + 1100*time.Millisecond) // the cached reset time passes
	n := len(*hits)
	for i := 0; i < 6; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("", ""))
	}
	used := false
	for _, got := range (*hits)[n:] {
		used = used || got == "acct-a"
	}
	if !used {
		t.Errorf("after its reset the account never came back: %v", (*hits)[n:])
	}
}

// When every account is spent, nothing is skipped: the caller gets the
// provider's own answer instead of an empty route.
func TestEverySpentAccountStillTries(t *testing.T) {
	stubClaudeQuota(t, func(string) QuotaWindow {
		return QuotaWindow{Name: "5h", UsedPct: 100, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	})
	up, hits := affinityServer(t, nil)
	h := affinityAPI(t, up)
	loadQuota(t, h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sessionRequest("", ""))
	if len(*hits) == 0 || rec.Code != http.StatusOK {
		t.Errorf("status %d, upstream calls %v: every account spent must still be tried", rec.Code, *hits)
	}
}
