package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

func declaredQuotaSetup(t *testing.T, quotaPath string, handler http.HandlerFunc) (*api, store.Connection) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := provider.Def{ID: "quotaco", BaseURL: srv.URL + "/v1", AuthHeader: "X-Key", AuthPrefix: "K ",
		Headers: map[string]string{"X-Extra": "1"}}
	if quotaPath != "" {
		d.QuotaURL = srv.URL + quotaPath
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	a, _ := newServer(s, nil, nil)
	// After newServer: it loads the declared providers of its own store.
	provider.SetDeclared([]provider.Def{d})
	t.Cleanup(func() { provider.SetDeclared(nil) })
	c, err := s.CreateConnection("quotaco", "main", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return a, c
}

// A declared provider with quotaUrl is read like a built-in one: the account's
// credential and the def's headers go along, and the answer is in intact's shape.
func TestDeclaredQuotaURLIsReadWithTheAccountCredential(t *testing.T) {
	var got http.Header
	a, c := declaredQuotaSetup(t, "/quota", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/quota" {
			http.NotFound(w, r)
			return
		}
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"plan":"Free","windows":[
			{"name":"Bonus Pack","usedPct":4,"used":"10","limit":"250","resetAt":"2026-11-01T06:02:06Z"},
			{"name":"no ratio","used":"3"},
			{"name":"","usedPct":10},
			{"name":"over","usedPct":150},
			{"name":"bad reset","usedPct":1,"resetAt":"next week"},
			{"name":"open","usedPct":-1,"unlimited":true}]}`))
	})

	q := a.quotaFor(context.Background(), c, true)
	if q.Error != "" || q.Source != "api" || q.Plan != "Free" {
		t.Fatalf("quota = %+v", q)
	}
	if got.Get("X-Key") != "K sec" || got.Get("X-Extra") != "1" {
		t.Errorf("headers = %v", got)
	}
	want := []QuotaWindow{
		{Name: "Bonus Pack", UsedPct: 4, Used: "10", Limit: "250", ResetAt: "2026-11-01T06:02:06Z"},
		{Name: "no ratio", UsedPct: -1, Used: "3"},
		{Name: "over", UsedPct: 100},
		{Name: "bad reset", UsedPct: 1},
		{Name: "open", UsedPct: -1, Unlimited: true},
	}
	if len(q.Windows) != len(want) {
		t.Fatalf("windows = %+v", q.Windows)
	}
	for i := range want {
		if q.Windows[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, q.Windows[i], want[i])
		}
	}

	fresh, err := a.freshQuota(context.Background(), c)
	if err != nil || fresh.Plan != "Free" {
		t.Errorf("freshQuota = %+v, %v", fresh, err)
	}
}

// used and limit may come as numbers, resetAt as epoch seconds or milliseconds.
func TestDeclaredQuotaURLReadsNumbersAndEpochResets(t *testing.T) {
	a, c := declaredQuotaSetup(t, "/quota", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"windows":[
			{"name":"numbers","usedPct":0,"used":10,"limit":250,"resetAt":1793512926},
			{"name":"millis","usedPct":0,"resetAt":1793512926000}]}`))
	})
	q := a.quotaFor(context.Background(), c, true)
	want := []QuotaWindow{
		{Name: "numbers", UsedPct: 0, Used: "10", Limit: "250", ResetAt: "2026-11-01T06:02:06Z"},
		{Name: "millis", UsedPct: 0, ResetAt: "2026-11-01T06:02:06Z"},
	}
	if q.Error != "" || len(q.Windows) != len(want) {
		t.Fatalf("quota = %+v", q)
	}
	for i := range want {
		if q.Windows[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, q.Windows[i], want[i])
		}
	}
}

// {accountId} in quotaUrl is filled from the id an account stores when the
// base URL asks for one; the account is added the way the dashboard adds it.
func TestDeclaredQuotaURLFillsTheAccountID(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"windows":[{"name":"credits","usedPct":5}]}`))
	}))
	t.Cleanup(srv.Close)
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	a, h := newServer(s, nil, nil)
	d := provider.Def{ID: "quotaco", BaseURL: srv.URL + "/accounts/{accountId}/v1",
		QuotaURL: srv.URL + "/accounts/{accountId}/quota"}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	provider.SetDeclared([]provider.Def{d})
	t.Cleanup(func() { provider.SetDeclared(nil) })

	req := loopbackRequest("POST", "/accounts", strings.NewReader("provider=quotaco&label=a&secret=sec&account_id=acct1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("create account: %d %s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListConnections()
	if len(list) != 1 || list[0].Meta["accountId"] != "acct1" {
		t.Fatalf("connections = %+v", list)
	}
	q := a.quotaFor(context.Background(), list[0], true)
	if q.Error != "" || path != "/accounts/acct1/quota" {
		t.Errorf("path = %q, quota = %+v", path, q)
	}

	// The id is one path segment: it cannot reach another path or a query.
	if err := s.SetMeta(list[0].ID, map[string]string{"accountId": "a/../b?x=1"}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	list, _ = s.ListConnections()
	path = ""
	if q := a.quotaFor(context.Background(), list[0], true); q.Error != "" || path != "/accounts/a/../b?x=1/quota" {
		t.Errorf("escaped path = %q, quota = %+v", path, q)
	}

	// An account without a stored id gets an error, not a call with an empty segment.
	c, err := s.CreateConnection("quotaco", "no-id", "sec")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if err := s.SetBaseURL(c.ID, srv.URL+"/accounts/x/v1"); err != nil {
		t.Fatalf("set base url: %v", err)
	}
	list, _ = s.ListConnections()
	for _, conn := range list {
		if conn.ID != c.ID {
			continue
		}
		path = ""
		if q := a.quotaFor(context.Background(), conn, true); !strings.Contains(q.Error, "account id") || path != "" {
			t.Errorf("no id: path = %q, quota = %+v", path, q)
		}
	}
}

// A redirect status and a 200 that is not JSON are errors, not an empty quota.
func TestDeclaredQuotaURLRedirectAndUnreadableAnswerAreErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"3xx":      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotModified) },
		"not json": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>login</html>")) },
	} {
		a, c := declaredQuotaSetup(t, "/quota", handler)
		q := a.quotaFor(context.Background(), c, true)
		if q.Error == "" || len(q.Windows) != 0 {
			t.Errorf("%s: quota = %+v", name, q)
		}
	}
}

func TestDeclaredQuotaURLFailureIsReported(t *testing.T) {
	a, c := declaredQuotaSetup(t, "/quota", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
	})
	q := a.quotaFor(context.Background(), c, true)
	if !strings.Contains(q.Error, "502") || q.Windows == nil || len(q.Windows) != 0 {
		t.Errorf("quota = %+v", q)
	}
}

// Without quotaUrl nothing is called: the account keeps the header fallback.
func TestDeclaredProviderWithoutQuotaURLIsNotCalled(t *testing.T) {
	called := false
	a, c := declaredQuotaSetup(t, "", func(w http.ResponseWriter, r *http.Request) { called = true })
	q := a.quotaFor(context.Background(), c, true)
	if called || q.Error != "" || q.Source != "" {
		t.Errorf("called = %v, quota = %+v", called, q)
	}
	if _, err := a.freshQuota(context.Background(), c); err == nil {
		t.Errorf("freshQuota without quotaUrl gave no error")
	}
}
