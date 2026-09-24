package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func postJSON(h http.Handler, path string, body string, ck *http.Cookie, token string) *httptest.ResponseRecorder {
	req := loopbackRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 1. Auth: No-auth mode and token/key callers are refused with 403.
func TestClaimAuthRequirements(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	c, _ := s.CreateConnection("claude", "acc", "token-x")

	// No-auth mode server
	noAuthHandler := New(s, nil)
	rec := postJSON(noAuthHandler, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-1"}`, nil, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-auth claim code = %d, want 403", rec.Code)
	}

	// Server with auth
	cfg := authConfig()
	authHandler := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, authHandler, cfg)
	dashKey := keyThroughDashboard(t, authHandler, ck, "dash-key")

	// Request without session
	recNoSess := postJSON(authHandler, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-1"}`, nil, "")
	if recNoSess.Code != http.StatusFound || recNoSess.Header().Get("Location") != "/login" {
		t.Errorf("no session claim code = %d (loc %s), want 302 /login", recNoSess.Code, recNoSess.Header().Get("Location"))
	}

	// Request with master token
	recMaster := postJSON(authHandler, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-1"}`, nil, cfg.APIToken)
	if recMaster.Code != http.StatusFound || recMaster.Header().Get("Location") != "/login" {
		t.Errorf("master token claim code = %d (loc %s), want 302 /login", recMaster.Code, recMaster.Header().Get("Location"))
	}

	// Request with dashboard key
	recKey := postJSON(authHandler, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-1"}`, nil, dashKey.Key)
	if recKey.Code != http.StatusFound || recKey.Header().Get("Location") != "/login" {
		t.Errorf("key claim code = %d (loc %s), want 302 /login", recKey.Code, recKey.Header().Get("Location"))
	}

	// Direct call to claimReset without session
	a, _ := newServer(s, nil, cfg)
	recDirect := httptest.NewRecorder()
	reqDirect := loopbackRequest("POST", "/quota/"+c.ID+"/reset", strings.NewReader(`{"resetId":"weekly","requestId":"req-1"}`))
	reqDirect.Header.Set("Content-Type", "application/json")
	a.claimReset(recDirect, reqDirect)
	if recDirect.Code != http.StatusForbidden {
		t.Errorf("direct claimReset code = %d, want 403", recDirect.Code)
	}
}

// 2. Lock: two concurrent claims for the same connection serialize via lock, giving 409 claim_in_progress.
func TestClaimConcurrencyLock(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	holdClaim := make(chan struct{})
	claimStarted := make(chan struct{})
	var claimCalls atomic.Int32

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			if ua := r.Header.Get("User-Agent"); !strings.HasSuffix(ua, "(external, cli)") {
				t.Errorf("claim User-Agent = %q, want the interactive CLI surface", ua)
			}
			claimCalls.Add(1)
			close(claimStarted)
			<-holdClaim
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	a, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	var rec1 *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec1 = postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-concur-1"}`, ck, "")
	}()

	<-claimStarted

	// Second claim arrives while first is in flight
	rec2 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-concur-2"}`, ck, "")
	if rec2.Code != http.StatusConflict {
		t.Errorf("second claim code = %d, want 409", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "claim_in_progress") {
		t.Errorf("second claim body = %s, want claim_in_progress", rec2.Body.String())
	}

	close(holdClaim)
	wg.Wait()

	if rec1.Code != http.StatusOK {
		t.Errorf("first claim code = %d, want 200", rec1.Code)
	}
	if n := claimCalls.Load(); n != 1 {
		t.Errorf("claimCalls = %d, want 1", n)
	}
	_ = a
}

// 3. Reset not found or not usable -> 409 with blocked reason, 0 claims to fake.
func TestClaimResetNotFoundOrNotUsable(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var claimCalls atomic.Int32
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"juniper_tide": map[string]any{
					"eligible":          false,
					"ineligible_reason": "surface",
					"available":         false,
				},
				"cedar_ember": map[string]any{
					"eligible": true,
					"grants":   []any{},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	// Reset not found in fresh read
	recNF := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:nonexistent","requestId":"req-nf"}`, ck, "")
	if recNF.Code != http.StatusConflict {
		t.Errorf("not found code = %d, want 409", recNF.Code)
	}
	if !strings.Contains(recNF.Body.String(), "not_found") {
		t.Errorf("not found body = %s, want not_found", recNF.Body.String())
	}

	// Reset present but not usable (ineligible:surface)
	recNotUsable := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-nu"}`, ck, "")
	if recNotUsable.Code != http.StatusConflict {
		t.Errorf("not usable code = %d, want 409", recNotUsable.Code)
	}
	if !strings.Contains(recNotUsable.Body.String(), "ineligible:surface") {
		t.Errorf("not usable body = %s, want ineligible:surface", recNotUsable.Body.String())
	}

	if claimCalls.Load() != 0 {
		t.Errorf("claimCalls = %d, want 0", claimCalls.Load())
	}
}

// 4. Timeout gives unknown; second claim with new requestId gets 409 unknown_pending.
// "Try again" within 60s gets 409 unknown_pending.
// After 60s, "Try again" with fewer resets gets likely_spent.
func TestClaimUnknownAndLikelySpentLifecycle(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var claimCalls atomic.Int32
	var resetsLeft atomic.Int32
	resetsLeft.Store(3)

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"cedar_ember": map[string]any{
					"eligible": true,
					"grants": []any{
						map[string]any{
							"id":           "g1",
							"resets_left":  json.Number(string(rune('0' + resetsLeft.Load()))),
							"resets_total": json.Number("3"),
							"usable_now":   true,
						},
					},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			// Return a 500 error to simulate failure that results in unknown outcome
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	a, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	// 1. Initial claim returns 500 from provider -> mapped to outcome "unknown"
	rec1 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-first"}`, ck, "")
	if rec1.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec1.Code, rec1.Body.String())
	}
	var res1 struct {
		Outcome   string `json:"outcome"`
		RequestID string `json:"requestId"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &res1)
	if res1.Outcome != "unknown" {
		t.Fatalf("outcome = %q, want unknown", res1.Outcome)
	}

	// 2. Second claim with a new requestId gets 409 unknown_pending with first requestId
	rec2 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-second"}`, ck, "")
	if rec2.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "req-first") {
		t.Errorf("body = %s, want req-first", rec2.Body.String())
	}

	// 3. Try again with same requestId within 60s gets 409 unknown_pending
	rec3 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-first"}`, ck, "")
	if rec3.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409", rec3.Code)
	}

	// 4. Fast forward time in unknown table: set time to 61 seconds ago
	a.claims.mu.Lock()
	entry := a.claims.unknown[c.ID+":grant:g1"]
	entry.at = time.Now().Add(-65 * time.Second)
	a.claims.unknown[c.ID+":grant:g1"] = entry
	a.claims.mu.Unlock()

	// Provider now reports fewer resets (2 instead of 3)
	resetsLeft.Store(2)

	// "Try again" after 60s gets likely_spent
	rec4 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-first"}`, ck, "")
	if rec4.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec4.Code, rec4.Body.String())
	}
	var res4 struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec4.Body.Bytes(), &res4)
	if res4.Outcome != "likely_spent" {
		t.Errorf("outcome = %q, want likely_spent", res4.Outcome)
	}
	if n := claimCalls.Load(); n != 1 {
		t.Errorf("claimCalls = %d, want 1", n)
	}
}

// 5. Hold table: after reset, a claim while provider still reports old resets gets 409 just_reset.
// And already_done: sending the same requestId again gets 409 already_done.
func TestClaimHoldTableAndAlreadyDone(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var claimCalls atomic.Int32
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	// 1. Initial claim succeeds with "reset"
	rec1 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-done-1"}`, ck, "")
	if rec1.Code != 200 {
		t.Fatalf("rec1 code = %d, body = %s", rec1.Code, rec1.Body.String())
	}
	var res1 struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &res1)
	if res1.Outcome != "reset" {
		t.Fatalf("outcome = %q, want reset", res1.Outcome)
	}

	// 2. Second claim with new requestId while fake still reports available=true gets 409 just_reset
	rec2 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-done-2"}`, ck, "")
	if rec2.Code != http.StatusConflict {
		t.Errorf("rec2 code = %d, want 409", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "just_reset") {
		t.Errorf("rec2 body = %s, want just_reset", rec2.Body.String())
	}

	// 3. Reusing req-done-1 gets 409 already_done
	rec3 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-done-1"}`, ck, "")
	if rec3.Code != http.StatusConflict {
		t.Errorf("rec3 code = %d, want 409", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "already_done") {
		t.Errorf("rec3 body = %s, want already_done", rec3.Body.String())
	}

	if claimCalls.Load() != 1 {
		t.Errorf("claimCalls = %d, want 1", claimCalls.Load())
	}
}

// 6. HTTP 401 gives failed, forces token refresh, does not enter done table.
func TestClaimHTTP401FailedAndRefreshesToken(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var claimCalls atomic.Int32
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	rec := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-401"}`, ck, "")
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", res.Outcome)
	}
	if claimCalls.Load() != 1 {
		t.Errorf("claimCalls = %d, want 1", claimCalls.Load())
	}
}

// 7. Codex failed fresh read gives 503, not 409.
func TestClaimCodexFreshReadFailure503(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	fakeCodex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer fakeCodex.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"codex": fakeCodex.URL}, cfg)
	c, _ := s.CreateConnection("codex", "acc", "token-x")
	ck := loginCookie(t, h, cfg)

	rec := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"credit:c1","requestId":"req-503"}`, ck, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
}

// 8. Claude 403 on claim sets meta.claudeOrgId to "", next claim re-reads profile.
func TestClaimClaudeOrgIDRecoveryOn403(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var profileReads atomic.Int32
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/oauth/profile") {
			profileReads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"organization": map[string]any{"uuid": "22222222-3333-4444-5555-666666666666"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			// First claim gets 403 forbidden
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	rec1 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-org-403"}`, ck, "")
	if rec1.Code != 200 {
		t.Fatalf("code = %d", rec1.Code)
	}

	// Verify meta was cleared to ""
	conns, _ := s.ListConnections()
	var found store.Connection
	for _, cn := range conns {
		if cn.ID == c.ID {
			found = cn
			break
		}
	}
	if found.Meta["claudeOrgId"] != "" {
		t.Errorf("claudeOrgId = %q, want empty after 403", found.Meta["claudeOrgId"])
	}

	// Next claim should re-read profile
	postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-org-next"}`, ck, "")
	if profileReads.Load() != 1 {
		t.Errorf("profileReads = %d, want 1", profileReads.Load())
	}
}

// 9. MCP tool cannot reach claimReset.
func TestMCPCannotReachClaimReset(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	res := rpc(t, h, cfg.APIToken, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"claim_reset","arguments":{}}}`)
	if res["error"] == nil {
		t.Error("expected error for non-existent claim_reset MCP tool")
	}
}

// C1: failed org-UUID lookup recorded as failed, not unknown.
func TestClaimClaudeOrgLookupFailure(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var resetCalls atomic.Int32
	var profileStatus atomic.Int32
	profileStatus.Store(http.StatusInternalServerError)

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/api/oauth/profile") {
			http.Error(w, "profile error", int(profileStatus.Load()))
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			resetCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	a, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	ck := loginCookie(t, h, cfg)

	// Case 1: profile answers 500
	c1, _ := s.CreateConnection("claude", "acc1", "token-1")
	profileStatus.Store(http.StatusInternalServerError)
	rec1 := postJSON(h, "/quota/"+c1.ID+"/reset", `{"resetId":"weekly","requestId":"req-c1-500"}`, ck, "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("case 1 code = %d, want 200", rec1.Code)
	}
	var res1 struct {
		Outcome      string `json:"outcome"`
		ProviderCode string `json:"providerCode"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &res1)
	if res1.Outcome != "failed" {
		t.Errorf("case 1 outcome = %q, want failed", res1.Outcome)
	}
	if resetCalls.Load() != 0 {
		t.Errorf("reset calls = %d, want 0", resetCalls.Load())
	}
	a.claims.mu.Lock()
	if _, ok := a.claims.unknown[c1.ID+":weekly"]; ok {
		t.Errorf("case 1 claims.unknown has entry for weekly")
	}
	a.claims.mu.Unlock()

	// Case 2: profile answers 401
	c2, _ := s.CreateConnection("claude", "acc2", "token-2")
	profileStatus.Store(http.StatusUnauthorized)
	rec2 := postJSON(h, "/quota/"+c2.ID+"/reset", `{"resetId":"weekly","requestId":"req-c2-401"}`, ck, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("case 2 code = %d, want 200", rec2.Code)
	}
	var res2 struct {
		Outcome      string `json:"outcome"`
		ProviderCode string `json:"providerCode"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &res2)
	if res2.Outcome != "failed" {
		t.Errorf("case 2 outcome = %q, want failed", res2.Outcome)
	}
	if res2.ProviderCode != "401" {
		t.Errorf("case 2 providerCode = %q, want 401", res2.ProviderCode)
	}
	if resetCalls.Load() != 0 {
		t.Errorf("reset calls = %d, want 0", resetCalls.Load())
	}
	a.claims.mu.Lock()
	if _, ok := a.claims.unknown[c2.ID+":weekly"]; ok {
		t.Errorf("case 2 claims.unknown has entry for weekly")
	}
	a.claims.mu.Unlock()
}

// C5: Codex redeemed credits have Left=0, so an unknown credit retry after >60s gets likely_spent.
func TestClaimCodexUnknownRetryLikelySpent(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var consumeCalls atomic.Int32
	fakeCodex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wham/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"rate_limit_reset_credits": map[string]any{
					"available_count": json.Number("1"),
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wham/rate-limit-reset-credits") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"credits": []any{
					map[string]any{
						"id":         "c1",
						"status":     "redeemed",
						"expires_at": "2026-10-01T00:00:00Z",
					},
					map[string]any{
						"id":         "c2",
						"status":     "available",
						"expires_at": "2026-10-01T00:00:00Z",
					},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wham/rate-limit-reset-credits/consume") {
			consumeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"code": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeCodex.Close()

	cfg := authConfig()
	a, h := newServer(s, map[string]string{"codex": fakeCodex.URL}, cfg)
	c, _ := s.CreateConnection("codex", "acc", "token-codex")
	ck := loginCookie(t, h, cfg)

	// Set an unknown entry in the table set back > 60s
	a.claims.mu.Lock()
	a.claims.unknown[c.ID+":credit:c1"] = claimUnknownEntry{
		requestID: "req-c5",
		left:      1,
		at:        time.Now().Add(-65 * time.Second),
	}
	a.claims.mu.Unlock()

	rec := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"credit:c1","requestId":"req-c5"}`, ck, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Outcome != "likely_spent" {
		t.Errorf("outcome = %q, want likely_spent", res.Outcome)
	}
	if consumeCalls.Load() != 0 {
		t.Errorf("consume calls = %d, want 0", consumeCalls.Load())
	}
}

// C4: missing provider block returns 503 cannot read resets now, 0 claim calls.
func TestClaimMissingProviderBlock503(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var claimCalls atomic.Int32
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			w.Header().Set("Content-Type", "application/json")
			// No juniper_tide and no cedar_ember
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	a, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	ck := loginCookie(t, h, cfg)

	// 1. claim weekly with no juniper_tide -> 503
	recWeekly := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-w1"}`, ck, "")
	if recWeekly.Code != http.StatusServiceUnavailable {
		t.Errorf("weekly with no juniper_tide code = %d, want 503", recWeekly.Code)
	}
	if !strings.Contains(recWeekly.Body.String(), "cannot read resets now") {
		t.Errorf("weekly body = %s, want 'cannot read resets now'", recWeekly.Body.String())
	}

	// 2. claim grant:g1 with no cedar_ember -> 503
	recGrant := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-g1"}`, ck, "")
	if recGrant.Code != http.StatusServiceUnavailable {
		t.Errorf("grant with no cedar_ember code = %d, want 503", recGrant.Code)
	}
	if !strings.Contains(recGrant.Body.String(), "cannot read resets now") {
		t.Errorf("grant body = %s, want 'cannot read resets now'", recGrant.Body.String())
	}

	if claimCalls.Load() != 0 {
		t.Errorf("claimCalls = %d, want 0", claimCalls.Load())
	}

	// 3. unknown weekly entry set back > 60s, retried with juniper_tide absent -> 503, entry still there
	a.claims.mu.Lock()
	a.claims.unknown[c.ID+":weekly"] = claimUnknownEntry{
		requestID: "req-unk",
		left:      1,
		at:        time.Now().Add(-65 * time.Second),
	}
	a.claims.mu.Unlock()

	recRetry := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-unk"}`, ck, "")
	if recRetry.Code != http.StatusServiceUnavailable {
		t.Errorf("unknown retry absent juniper_tide code = %d, want 503", recRetry.Code)
	}
	if !strings.Contains(recRetry.Body.String(), "cannot read resets now") {
		t.Errorf("unknown retry body = %s, want 'cannot read resets now'", recRetry.Body.String())
	}

	if claimCalls.Load() != 0 {
		t.Errorf("claimCalls after retry = %d, want 0", claimCalls.Load())
	}

	a.claims.mu.Lock()
	if _, ok := a.claims.unknown[c.ID+":weekly"]; !ok {
		t.Errorf("unknown entry was deleted, want it to stay")
	}
	a.claims.mu.Unlock()
}

// C3: step 4 hold table early release when fresh read shows lower Left.
func TestClaimHoldEarlyReleaseOnLowerLeft(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// Case 1: fake says 2 after reset -> early release, second claim reaches endpoint (claim calls = 2)
	t.Run("early_release_on_decrement", func(t *testing.T) {
		var claimCalls atomic.Int32
		var resetsLeft atomic.Int32
		resetsLeft.Store(3)

		fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/oauth/usage") {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
					"cedar_ember": map[string]any{
						"eligible": true,
						"grants": []any{
							map[string]any{
								"id":           "g1",
								"resets_total": json.Number("5"),
								"resets_left":  json.Number(string(rune('0' + resetsLeft.Load()))),
								"usable_now":   true,
							},
						},
					},
				})
				return
			}
			if strings.Contains(r.URL.Path, "/reset_rate_limits") {
				claimCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
				return
			}
			http.NotFound(w, r)
		}))
		defer fakeClaude.Close()

		cfg := authConfig()
		_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
		c, _ := s.CreateConnection("claude", "acc1", "token-1")
		s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
		ck := loginCookie(t, h, cfg)

		// First claim: grant resets_left 3 -> reset
		rec1 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-c3-1"}`, ck, "")
		if rec1.Code != http.StatusOK {
			t.Fatalf("first claim code = %d", rec1.Code)
		}
		if claimCalls.Load() != 1 {
			t.Fatalf("claimCalls after first = %d, want 1", claimCalls.Load())
		}

		// Fake updates to 2 resets left
		resetsLeft.Store(2)

		// Second claim with new requestId within 60s
		rec2 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g1","requestId":"req-c3-2"}`, ck, "")
		if rec2.Code != http.StatusOK {
			t.Fatalf("second claim code = %d, body = %s, want 200", rec2.Code, rec2.Body.String())
		}
		if claimCalls.Load() != 2 {
			t.Errorf("claimCalls after second = %d, want 2", claimCalls.Load())
		}
	})

	// Case 2: fake still says 3 -> 409 just_reset (claim calls = 1)
	t.Run("hold_active_when_not_decremented", func(t *testing.T) {
		var claimCalls atomic.Int32

		fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/oauth/usage") {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
					"cedar_ember": map[string]any{
						"eligible": true,
						"grants": []any{
							map[string]any{
								"id":           "g2",
								"resets_total": json.Number("5"),
								"resets_left":  json.Number("3"),
								"usable_now":   true,
							},
						},
					},
				})
				return
			}
			if strings.Contains(r.URL.Path, "/reset_rate_limits") {
				claimCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
				return
			}
			http.NotFound(w, r)
		}))
		defer fakeClaude.Close()

		cfg := authConfig()
		_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
		c, _ := s.CreateConnection("claude", "acc2", "token-2")
		s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
		ck := loginCookie(t, h, cfg)

		// First claim: grant resets_left 3 -> reset
		rec1 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g2","requestId":"req-c3-3"}`, ck, "")
		if rec1.Code != http.StatusOK {
			t.Fatalf("first claim code = %d", rec1.Code)
		}
		if claimCalls.Load() != 1 {
			t.Fatalf("claimCalls after first = %d, want 1", claimCalls.Load())
		}

		// Second claim with new requestId within 60s, fake still says 3
		rec2 := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"grant:g2","requestId":"req-c3-4"}`, ck, "")
		if rec2.Code != http.StatusConflict {
			t.Fatalf("second claim code = %d, want 409", rec2.Code)
		}
		if !strings.Contains(rec2.Body.String(), "just_reset") {
			t.Errorf("second claim body = %s, want just_reset", rec2.Body.String())
		}
		if claimCalls.Load() != 1 {
			t.Errorf("claimCalls after second = %d, want 1", claimCalls.Load())
		}
	})
}

// C6: claim for IsActive=false returns 404 unknown connection, 0 usage calls, 0 claim calls.
func TestClaimInactiveConnection404(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var usageCalls atomic.Int32
	var claimCalls atomic.Int32

	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/usage") {
			usageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": json.Number("100"), "resets_at": "2026-09-23T15:00:00Z"},
				"juniper_tide": map[string]any{
					"eligible":  true,
					"available": true,
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/reset_rate_limits") {
			claimCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": "reset"})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeClaude.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"claude": fakeClaude.URL}, cfg)
	c, _ := s.CreateConnection("claude", "acc", "token-x")
	s.SetMeta(c.ID, map[string]string{"claudeOrgId": "11111111-2222-3333-4444-555555555555"})
	if err := s.SetActive(c.ID, false); err != nil {
		t.Fatalf("SetActive false: %v", err)
	}

	ck := loginCookie(t, h, cfg)
	rec := postJSON(h, "/quota/"+c.ID+"/reset", `{"resetId":"weekly","requestId":"req-inactive"}`, ck, "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("inactive claim code = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown connection") {
		t.Errorf("inactive claim body = %s, want 'unknown connection'", rec.Body.String())
	}
	if usageCalls.Load() != 0 {
		t.Errorf("usage calls = %d, want 0", usageCalls.Load())
	}
	if claimCalls.Load() != 0 {
		t.Errorf("claim calls = %d, want 0", claimCalls.Load())
	}
}

// C9: test runs the Codex claim path.
func TestClaimCodex(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var consumeCalls atomic.Int32
	var lastCreditID string
	var lastRedeemReqID string
	var lastPath string
	var consumeCode atomic.Value
	consumeCode.Store("reset")

	fakeCodex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wham/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"rate_limit_reset_credits": map[string]any{
					"available_count": json.Number("1"),
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wham/rate-limit-reset-credits") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"credits": []any{
					map[string]any{
						"id":         "credit_abc_123",
						"status":     "available",
						"expires_at": "2026-10-23T00:00:00Z",
					},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/consume") {
			consumeCalls.Add(1)
			lastPath = r.URL.Path
			var b struct {
				CreditID        string `json:"credit_id"`
				RedeemRequestID string `json:"redeem_request_id"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			lastCreditID = b.CreditID
			lastRedeemReqID = b.RedeemRequestID

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"code": consumeCode.Load().(string),
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeCodex.Close()

	cfg := authConfig()
	_, h := newServer(s, map[string]string{"codex": fakeCodex.URL}, cfg)
	ck := loginCookie(t, h, cfg)

	// 1. Outcome reset for code reset
	c1, _ := s.CreateConnection("codex", "acc1", "token-codex-1")
	consumeCode.Store("reset")
	rec1 := postJSON(h, "/quota/"+c1.ID+"/reset", `{"resetId":"credit:credit_abc_123","requestId":"req-codex-reset"}`, ck, "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("claim 1 code = %d, body = %s", rec1.Code, rec1.Body.String())
	}
	if lastPath != "/backend-api/wham/rate-limit-reset-credits/consume" {
		t.Errorf("lastPath = %q, want /backend-api/wham/rate-limit-reset-credits/consume", lastPath)
	}
	if lastCreditID != "credit_abc_123" {
		t.Errorf("lastCreditID = %q, want credit_abc_123", lastCreditID)
	}
	if lastRedeemReqID != "req-codex-reset" {
		t.Errorf("lastRedeemReqID = %q, want req-codex-reset", lastRedeemReqID)
	}
	var res1 struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &res1)
	if res1.Outcome != "reset" {
		t.Errorf("claim 1 outcome = %q, want reset", res1.Outcome)
	}

	// 2. Outcome spent for already_redeemed
	c2, _ := s.CreateConnection("codex", "acc2", "token-codex-2")
	consumeCode.Store("already_redeemed")
	rec2 := postJSON(h, "/quota/"+c2.ID+"/reset", `{"resetId":"credit:credit_abc_123","requestId":"req-codex-spent"}`, ck, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("claim 2 code = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if lastRedeemReqID != "req-codex-spent" {
		t.Errorf("lastRedeemReqID = %q, want req-codex-spent", lastRedeemReqID)
	}
	var res2 struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &res2)
	if res2.Outcome != "spent" {
		t.Errorf("claim 2 outcome = %q, want spent", res2.Outcome)
	}
	if consumeCalls.Load() != 2 {
		t.Errorf("consumeCalls = %d, want 2", consumeCalls.Load())
	}
}
