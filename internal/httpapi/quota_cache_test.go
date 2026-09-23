package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// A fresh read A starts, a refresh=1 read B starts, A finishes last. The cache holds the data of B.
func TestQuotaSequenceAandBOrder(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	t.Cleanup(func() { quotaFetchers["claude"] = origClaude })

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	a, _ := newServer(s, nil, nil)
	c, err := s.CreateConnection("claude", "test-claude", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	var aStarted, bCanFinish, bDone, aCanFinish chan struct{}
	aStarted = make(chan struct{})
	bCanFinish = make(chan struct{})
	bDone = make(chan struct{})
	aCanFinish = make(chan struct{})

	var callCount atomic.Int32
	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		n := callCount.Add(1)
		if n == 1 {
			// Read A
			close(aStarted)
			<-aCanFinish
			return AccountQuota{Plan: "plan-A", Windows: []QuotaWindow{}}, nil
		}
		// Read B
		<-bCanFinish
		return AccountQuota{Plan: "plan-B", Windows: []QuotaWindow{}}, nil
	}

	var resA, resB AccountQuota
	var wg sync.WaitGroup

	// Start read A (freshQuota or quotaFor)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		resA, err = a.freshQuota(context.Background(), c)
		if err != nil {
			t.Errorf("freshQuota A: %v", err)
		}
	}()

	// Wait until A has started
	<-aStarted

	// Start read B (refresh=1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resB = a.quotaFor(context.Background(), c, true)
		close(bDone)
	}()

	// Let B finish first
	close(bCanFinish)
	<-bDone

	// Now let A finish
	close(aCanFinish)
	wg.Wait()

	if resA.Plan != "plan-A" || resB.Plan != "plan-B" {
		t.Fatalf("unexpected results: resA=%+v resB=%+v", resA, resB)
	}

	// Cache must hold data of B because B started after A (higher sequence number)
	a.quota.mu.Lock()
	cached := a.quota.m[c.ID]
	a.quota.mu.Unlock()
	if cached.Plan != "plan-B" {
		t.Errorf("cache holds %q, want plan-B", cached.Plan)
	}
}

// 10 parallel GET /api/quota and 10 parallel classify429 calls on an expired entry make exactly one usage call.
func TestQuotaSingleFlightTenPlusTenMissesOneCall(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	t.Cleanup(func() { quotaFetchers["claude"] = origClaude })

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)

	c, err := s.CreateConnection("claude", "test-claude", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	var calls atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		time.Sleep(50 * time.Millisecond)
		return AccountQuota{Plan: "single-flight-plan", Windows: []QuotaWindow{{Name: "5h", UsedPct: 10}}}, nil
	}

	// Expired or empty entry in cache
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := loopbackRequest("GET", "/api/quota", nil)
			req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Errorf("/api/quota: code %d", rec.Code)
			}
		}()
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.classify429(1, c, "claude-3-7-sonnet", 0)
		}()
	}

	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("quota provider calls = %d, want exactly 1", n)
	}
}

// 20 calls to GET /api/quota?refresh=1 within 60s make zero calls at fake, once with key and once with master token.
// A claim started right after them still makes its step-4 read.
func TestQuotaRefreshRouteRestrictionKeyAndMasterToken(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	t.Cleanup(func() { quotaFetchers["claude"] = origClaude })

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	c, err := s.CreateConnection("claude", "test-claude", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	var calls atomic.Int32
	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		calls.Add(1)
		return AccountQuota{Plan: "initial", Windows: []QuotaWindow{}}, nil
	}

	// Warm cache
	_ = a.quotaFor(context.Background(), c, false)
	if calls.Load() != 1 {
		t.Fatalf("initial call not made")
	}

	dashKey := keyThroughDashboard(t, h, ck, "dash-key")

	// 20 calls with dashboard key
	for i := 0; i < 20; i++ {
		rec := getAs(h, "/api/quota?refresh=1", dashKey.Key)
		if rec.Code != 200 {
			t.Errorf("key call %d: %d", i, rec.Code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("after key calls, provider calls = %d, want 1", n)
	}

	// 20 calls with master token
	for i := 0; i < 20; i++ {
		rec := getAs(h, "/api/quota?refresh=1", cfg.APIToken)
		if rec.Code != 200 {
			t.Errorf("master token call %d: %d", i, rec.Code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("after master token calls, provider calls = %d, want 1", n)
	}

	// Fresh read (step 4 of claim) must bypass cache and hit provider
	q, err := a.freshQuota(context.Background(), c)
	if err != nil {
		t.Fatalf("freshQuota: %v", err)
	}
	if q.Plan != "initial" {
		t.Errorf("got plan %q", q.Plan)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("freshQuota call count = %d, want 2", n)
	}
}

// GET /quota?connection=<id> filter returns only the requested connection, and empty list for unknown id.
func TestQuotaConnectionFilter(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	origCodex := quotaFetchers["codex"]
	t.Cleanup(func() {
		quotaFetchers["claude"] = origClaude
		quotaFetchers["codex"] = origCodex
	})

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	_, h := newServer(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	c1, _ := s.CreateConnection("claude", "acc1", "sec1")
	c2, _ := s.CreateConnection("codex", "acc2", "sec2")

	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		return AccountQuota{Windows: []QuotaWindow{}}, nil
	}
	quotaFetchers["codex"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		return AccountQuota{Windows: []QuotaWindow{}}, nil
	}

	// Request c1
	rec1 := getWithCookie(h, "/quota?connection="+c1.ID, ck)
	if rec1.Code != 200 {
		t.Fatalf("code %d: %s", rec1.Code, rec1.Body.String())
	}
	var res1 struct {
		Accounts []AccountQuota `json:"accounts"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &res1)
	if len(res1.Accounts) != 1 || res1.Accounts[0].ConnectionID != c1.ID {
		t.Errorf("wanted only c1, got %+v (c2=%s)", res1.Accounts, c2.ID)
	}

	// Unknown connection
	recUnknown := getWithCookie(h, "/quota?connection=unknown", ck)
	if recUnknown.Code != 200 {
		t.Fatalf("code %d: %s", recUnknown.Code, recUnknown.Body.String())
	}
	var resUnknown struct {
		Accounts []AccountQuota `json:"accounts"`
	}
	json.Unmarshal(recUnknown.Body.Bytes(), &resUnknown)
	if len(resUnknown.Accounts) != 0 {
		t.Errorf("wanted empty accounts, got %+v", resUnknown.Accounts)
	}
}

// JSON never has "windows":null or "resets":null.
func TestQuotaWindowsAndResetsNeverNullInJSON(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	t.Cleanup(func() { quotaFetchers["claude"] = origClaude })

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	_, h := newServer(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	_, err = s.CreateConnection("claude", "acc1", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		return AccountQuota{}, nil
	}

	rec := getWithCookie(h, "/quota", ck)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"windows":null`) {
		t.Errorf("body contains windows:null: %s", body)
	}
	if strings.Contains(body, `"resets":null`) {
		t.Errorf("body contains resets:null: %s", body)
	}
	if !strings.Contains(body, `"windows":[]`) {
		t.Errorf("body missing windows:[]: %s", body)
	}
	if !strings.Contains(body, `"resets":[]`) {
		t.Errorf("body missing resets:[]: %s", body)
	}
}

// MCP get_quota ignores refresh: true.
func TestMCPGetQuotaIgnoresRefresh(t *testing.T) {
	origClaude := quotaFetchers["claude"]
	t.Cleanup(func() { quotaFetchers["claude"] = origClaude })

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	cfg := authConfig()
	a, h := newServer(s, nil, cfg)

	c, _ := s.CreateConnection("claude", "acc1", "sec")

	var calls atomic.Int32
	quotaFetchers["claude"] = func(_ *api, ctx context.Context, _ store.Connection, _ string) (AccountQuota, error) {
		calls.Add(1)
		return AccountQuota{Plan: "mcp-test", Windows: []QuotaWindow{}}, nil
	}

	// Warm cache
	_ = a.quotaFor(context.Background(), c, false)
	if calls.Load() != 1 {
		t.Fatalf("initial fetch failed")
	}

	// MCP call with refresh: true
	res := rpc(t, h, cfg.APIToken, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_quota","arguments":{"refresh":true}}}`)
	text, isErr := toolText(t, res)
	if isErr {
		t.Fatalf("mcp tool error: %s", text)
	}
	if calls.Load() != 1 {
		t.Errorf("MCP get_quota with refresh:true triggered provider fetch (calls=%d, want 1)", calls.Load())
	}
}
