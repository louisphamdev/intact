package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

// An account whose quota is spent is skipped while its window is ahead.
func TestSpentAccountIsSkippedUntilItsReset(t *testing.T) {
	resetAt := time.Now().Add(time.Hour)
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

func spentAPI(t *testing.T) *api {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a, _ := newServer(s, nil, nil)
	return a
}

func setQuota(a *api, id string, w ...QuotaWindow) {
	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	if a.quota.m == nil {
		a.quota.m = map[string]AccountQuota{}
	}
	a.quota.m[id] = AccountQuota{ConnectionID: id, Windows: w}
}

func at(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

func near(t *testing.T, got time.Time, d time.Duration) {
	t.Helper()
	if want := time.Now().Add(d); got.Before(want.Add(-time.Minute)) || got.After(want.Add(time.Minute)) {
		t.Errorf("until = %v, want about now+%v", got, d)
	}
}

// The reset that matters is the one of the window that made the model spent.
func TestSpentUntilFollowsTheDecidingWindow(t *testing.T) {
	a := spentAPI(t)
	setQuota(a, "c", QuotaWindow{Name: "model gemini-a", UsedPct: 100, ResetAt: at(120 * time.Hour)},
		QuotaWindow{Name: "model gemini-b", UsedPct: 100, ResetAt: at(time.Hour)})
	near(t, a.spentUntil("c", "gemini-b"), time.Hour)

	setQuota(a, "c", QuotaWindow{Name: "5h", UsedPct: 100, ResetAt: at(2 * time.Hour)},
		QuotaWindow{Name: "7d", UsedPct: 100, ResetAt: at(120 * time.Hour)})
	near(t, a.spentUntil("c", "claude-opus-5-5"), 120*time.Hour)

	setQuota(a, "c", QuotaWindow{Name: "model gemini-a", UsedPct: 100, ResetAt: at(120 * time.Hour)})
	if u := a.spentUntil("c", "other-model"); !u.IsZero() {
		t.Errorf("a model with no matching window is spent until %v", u)
	}
	setQuota(a, "c", QuotaWindow{Name: "5h", UsedPct: 100})
	if u := a.spentUntil("c", "m"); !u.IsZero() {
		t.Errorf("no reset time known, yet spent until %v: an account must never be parked for good", u)
	}
	setQuota(a, "c", QuotaWindow{Name: "5h", UsedPct: 100, ResetAt: at(-time.Minute)})
	if u := a.spentUntil("c", "m"); !u.IsZero() {
		t.Errorf("the reset passed, yet spent until %v", u)
	}
	setQuota(a, "c", QuotaWindow{Name: "model X", UsedPct: 104, ResetAt: at(2 * time.Hour)})
	near(t, a.spentUntil("c", "X"), 2*time.Hour)
	setQuota(a, "c", QuotaWindow{Name: "model X", UsedPct: 100}, QuotaWindow{Name: "model Y", UsedPct: 100, ResetAt: at(time.Hour)})
	if u := a.spentUntil("c", "X"); !u.IsZero() {
		t.Errorf("X has no reset time; an unrelated window gave %v", u)
	}
	setQuota(a, "c", QuotaWindow{Name: "5h", UsedPct: -1})
	if u := a.spentUntil("c", "m"); !u.IsZero() {
		t.Errorf("unknown quota counted as spent until %v", u)
	}
}

// C1: a session hit keeps the rotation order with standby accounts last.
func TestSessionHitKeepsStandbyLast(t *testing.T) {
	fail := map[string]int{}
	up, hits := affinityServer(t, fail)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("claude", "A", "acct-a")
	cs, _ := s.CreateConnection("claude", "S", "acct-s")
	cb, _ := s.CreateConnection("claude", "B", "acct-b")
	s.CreateConnection("claude", "C", "acct-c") // store order A, S, B, C
	s.SetStandby(cs.ID, true)
	a, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	// b is not the rotation's first pick, so only the session lookup sends it first.
	a.sessions.set(sessionKey(sessionRequest("sess-sb", ""), nil), cb.ID)
	for _, k := range []string{"acct-a", "acct-b", "acct-c"} {
		fail[k] = http.StatusTooManyRequests
	}
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-sb", ""))
	if got := strings.Join(*hits, ","); got != "acct-b,acct-a,acct-c,acct-s" {
		t.Errorf("hits %s, want acct-b,acct-a,acct-c,acct-s: the session account, the normal ones, the standby last", got)
	}
	*hits = nil
	clear(fail)
	a.sessions.set(sessionKey(sessionRequest("sess-on-standby", ""), nil), cs.ID)
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-on-standby", ""))
	if len(*hits) == 0 || (*hits)[0] == "acct-s" {
		t.Errorf("a session pinned to a standby went there while normal accounts are healthy: %v", *hits)
	}
}

// C3: a huge session id is stored as a short digest; an idle entry is gone.
func TestSessionKeysAreBounded(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	k := sessionKey(sessionRequest(long, ""), nil)
	k2 := sessionKey(sessionRequest(long[:256]+"y"+long[257:], ""), nil)
	if len(k) > 70 || !strings.HasPrefix(k, "s:") || k == k2 {
		t.Errorf("a 100 KB session id gave %q (%d bytes); a different id gave the same key: %v", k[:min(len(k), 80)], len(k), k == k2)
	}
	var f affinity
	f.set("k", "conn")
	f.mu.Lock()
	e := f.m["k"]
	e.seen = time.Now().Add(-2 * affinityTTL)
	f.m["k"] = e
	f.mu.Unlock()
	if got := f.get("k"); got != "" {
		t.Errorf("an idle entry still pins %q", got)
	}
	f.mu.Lock()
	_, kept := f.m["k"]
	f.mu.Unlock()
	if kept {
		t.Error("an idle entry stays in the map")
	}
	for i := 0; i < affinityMax+10; i++ {
		f.set(fmt.Sprintf("k%d", i), "c")
	}
	f.mu.Lock()
	n, _ := len(f.m), 0
	_, newest := f.m[fmt.Sprintf("k%d", affinityMax+9)]
	f.mu.Unlock()
	if n > affinityMax || !newest {
		t.Errorf("map holds %d entries (max %d), newest present %v", n, affinityMax, newest)
	}
}

// C5: only an answer below 400 pins the session.
func TestOnlyASuccessPinsTheSession(t *testing.T) {
	fail := map[string]int{"acct-a": 400, "acct-b": 400, "acct-c": 400}
	up, hits := affinityServer(t, fail)
	h := affinityAPI(t, up)
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-400", ""))
	if len(*hits) != 1 {
		t.Fatalf("a 400 is not retried, yet hits = %v", *hits)
	}
	bad := (*hits)[0]
	for k := range fail {
		delete(fail, k)
	}
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-400", ""))
	if got := (*hits)[1]; got == bad {
		t.Errorf("a 400 answer pinned the session to %s", bad)
	}
}

// The first real 429 parks the account for the next request, even when the
// quota cache still says quota is left; a burst of 429s reads the quota at
// most once per account per 30 s, and routing itself never reads it.
func TestFirst429ParksTheAccountAtOnce(t *testing.T) {
	var reads atomic.Int32
	stubClaudeQuota(t, func(label string) QuotaWindow {
		reads.Add(1) // every account: routing must read none
		if label == "a" {
			return QuotaWindow{Name: "5h", UsedPct: 100, ResetAt: at(time.Hour)}
		}
		return QuotaWindow{Name: "5h", UsedPct: 50}
	})
	fail := map[string]int{"acct-a": http.StatusTooManyRequests}
	up, hits := affinityServer(t, fail)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { s.Close() })
	conns := map[string]string{}
	for _, l := range []string{"a", "b", "c"} {
		c, _ := s.CreateConnection("claude", l, "acct-"+l)
		conns[l] = c.ID
	}
	a, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	old := time.Now().Add(-time.Minute / 2).UTC().Format(time.RFC3339) // read before the 429s, still fresh
	for l, id := range conns {
		a.quota.m[id] = AccountQuota{ConnectionID: id, Provider: "claude", Label: l, FetchedAt: old,
			Windows: []QuotaWindow{{Name: "5h", UsedPct: 50}}}
	}
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("", ""))
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.spentUntil(conns["a"], "m").IsZero() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	n := len(*hits)
	for i := 0; i < 6; i++ {
		h.ServeHTTP(httptest.NewRecorder(), sessionRequest("", ""))
	}
	for _, got := range (*hits)[n:] {
		if got == "acct-a" {
			t.Fatalf("after its first 429 the spent account was called again: %v", (*hits)[n:])
		}
	}
	if r := reads.Load(); r != 1 {
		t.Errorf("quota was read %d times over all accounts, want 1: the forced read for a, none from routing", r)
	}
}

// Headers a proxy in front of intact adds (cloudflared, a load balancer) name
// the person behind the request; they never reach a provider.
func TestOutboundDropsProxyHeaders(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "a", "k")
	h := New(s, map[string]string{"claude": up.URL})
	r := sessionRequest("", "")
	for k, v := range map[string]string{"Cf-Connecting-Ip": "203.0.113.7", "Cf-Ipcountry": "VN", "Cf-Ray": "r", "Cf-Visitor": "{}",
		"Cf-Warp-Tag-Id": "w", "Cdn-Loop": "cloudflare", "X-Forwarded-For": "203.0.113.7", "X-Forwarded-Proto": "https",
		"X-Real-Ip": "203.0.113.7", "Forwarded": "for=203.0.113.7", "True-Client-Ip": "203.0.113.7", "Cookie": "sid=1",
		"X-Request-Id": "keep-me"} {
		r.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
	for _, k := range []string{"Cf-Connecting-Ip", "Cf-Ipcountry", "Cf-Ray", "Cf-Visitor", "Cf-Warp-Tag-Id", "Cdn-Loop",
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded", "True-Client-Ip", "Cookie"} {
		if v := got.Get(k); v != "" {
			t.Errorf("%s = %q reached the provider", k, v)
		}
	}
	if got.Get("X-Request-Id") != "keep-me" {
		t.Error("a client header that names no person was dropped")
	}
}

// A plain metadata.user_id names a user, not a conversation: no affinity from
// it. Older Claude Code put the session inside it (..._session_<uuid>).
func TestPlainUserIDNamesNoSession(t *testing.T) {
	r := sessionRequest("", `"user-123"`)
	if k := sessionKey(r, mustBody(r)); k != "" {
		t.Errorf("plain user id gave session key %q", k)
	}
	r = sessionRequest("", `"user_ab12_account_cd34_session_5f0e8c1a-1b2c-4d5e-8f90-123456789abc"`)
	if k := sessionKey(r, mustBody(r)); !strings.Contains(k, "5f0e8c1a-1b2c-4d5e-8f90-123456789abc") {
		t.Errorf("legacy user id gave session key %q, want its session uuid", k)
	}
}

func mustBody(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// Prompt openings of Claude Code 2.1.287's compaction requests.
const (
	compactFull    = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nYour task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions."
	compactUpTo    = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nYour task is to create a detailed summary of this conversation. This summary will be placed at the start of a continuing session; newer messages that build on this context will follow after your summary."
	compactRecent  = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nYour task is to create a detailed summary of the RECENT portion of the conversation — the messages that follow earlier retained context."
	compactHistory = `[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"ok"}]}]`
)

func compactRequest(session, prompt string) *http.Request {
	last, _ := json.Marshal([]map[string]string{{"type": "text", "text": prompt}})
	// Claude Code 2.1.287 puts a system message after the prompt.
	msgs := strings.TrimSuffix(compactHistory, "]") + `,{"role":"user","content":` + string(last) + `},{"role":"system","content":"<total_tokens>1 tokens left</total_tokens>"}]`
	r := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m","max_tokens":5,"messages":`+msgs+`}`))
	r.Header.Set("User-Agent", "claude-cli/2.1.287 (external, cli)")
	r.Header.Set("X-Claude-Code-Session-Id", session)
	return r
}

// A compaction replaces the history the cache was built on, so the session
// after it is free to take the rotation; the compaction itself still reads
// the old cache on its home account.
func TestCompactFreesTheSession(t *testing.T) {
	for name, prompt := range map[string]string{"full": compactFull, "up_to": compactUpTo} {
		t.Run(name, func(t *testing.T) {
			up, hits := affinityServer(t, nil)
			h := affinityAPI(t, up)
			h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-c", ""))
			home := (*hits)[0]
			h.ServeHTTP(httptest.NewRecorder(), compactRequest("sess-c", prompt))
			if got := (*hits)[1]; got != home {
				t.Fatalf("the compaction went to %s, want its home %s for the cache hit", got, home)
			}
			h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-c", ""))
			moved := (*hits)[2]
			if moved == home {
				t.Fatalf("after the compaction the session stayed on %s: %v", home, *hits)
			}
			h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-c", ""))
			if got := (*hits)[3]; got != moved {
				t.Errorf("the compacted session did not pin to its new account %s: %v", moved, *hits)
			}
		})
	}
}

// A compaction of the recent part keeps the earlier messages, and so the
// cached prefix: the session stays home.
func TestRecentCompactKeepsTheSession(t *testing.T) {
	up, hits := affinityServer(t, nil)
	h := affinityAPI(t, up)
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-r", ""))
	h.ServeHTTP(httptest.NewRecorder(), compactRequest("sess-r", compactRecent))
	h.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess-r", ""))
	if (*hits)[0] != (*hits)[1] || (*hits)[1] != (*hits)[2] {
		t.Errorf("a recent-part compaction moved the session: %v", *hits)
	}
}

// Only the last user message asks for the compaction; the same text earlier in
// the history (a quoted prompt) is not one.
func TestCompactIsReadFromTheLastMessageOnly(t *testing.T) {
	quoted, _ := json.Marshal(compactFull)
	cases := map[string]struct {
		body string
		want bool
	}{
		"string content":  {`{"messages":[{"role":"user","content":` + string(quoted) + `}]}`, true},
		"quoted earlier":  {`{"messages":[{"role":"user","content":` + string(quoted) + `},{"role":"assistant","content":"x"},{"role":"user","content":"go on"}]}`, false},
		"system after it": {`{"messages":[{"role":"user","content":` + string(quoted) + `},{"role":"system","content":"x"}]}`, true},
		"plain request":   {`{"messages":[{"role":"user","content":"hi"}]}`, false},
		"no messages":     {`{}`, false},
		"not json":        {`not json`, false},
	}
	for name, c := range cases {
		if got := compactRestarts([]byte(c.body)); got != c.want {
			t.Errorf("%s: compactRestarts = %v, want %v", name, got, c.want)
		}
	}
}
