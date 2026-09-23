package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

func TestProxyRefreshesExpiredOAuthToken(t *testing.T) {
	// The token endpoint hands back a new access token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"fresh-token","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	// The provider echoes the Authorization it received.
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "Personal", "stale-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt",
		TokenURL:     tokenSrv.URL,
		ClientID:     "cid",
		ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), // expired
	})

	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))

	if gotAuth != "Bearer fresh-token" {
		t.Fatalf("upstream saw auth %q, want the refreshed Bearer fresh-token", gotAuth)
	}
	if sec, _ := s.Secret(c.ID); sec != "fresh-token" {
		t.Errorf("stored secret = %q, want the refreshed token", sec)
	}
}

func TestProxyKeepsValidOAuthToken(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "P", "good-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt", TokenURL: "http://127.0.0.1:0/never",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), // still valid
	})
	h := NewWithAuth(s, map[string]string{"claude": up.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))
	if gotAuth != "Bearer good-token" {
		t.Errorf("auth=%q, want the existing token (no refresh)", gotAuth)
	}
}

// waitFor reads one value, or fails the test. A refresh that hangs must not
// hang the suite.
func waitFor[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(10 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", what)
		return zero
	}
}

// The provider rotates the refresh token while the caller disconnects. The
// exchange must finish on its own context, or the rotated token is lost and the
// account is dead. T1-5 (a), (c) and T2-4.
func TestRefreshOutlivesACancelledCaller(t *testing.T) {
	var n int32
	arrived := make(chan struct{})
	release := make(chan struct{})
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		i := atomic.AddInt32(&n, 1)
		fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"rotated-%d","expires_in":3600}`, i, i)
	}))
	defer tokenSrv.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("claude", "P", "stale-token")
	s.SetOAuth(c.ID, store.OAuthCreds{RefreshToken: "rt-0", TokenURL: tokenSrv.URL, ClientID: "cid",
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	a, _ := newServer(s, nil, nil)

	type result struct {
		tok string
		ok  bool
	}

	// secretFor: the caller goes away while the token endpoint still holds the
	// request.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() {
		tok, err := a.secretFor(ctx, c.ID)
		done <- result{tok, err == nil}
	}()
	waitFor(t, arrived, "the token endpoint to receive the secretFor exchange")
	cancel()
	release <- struct{}{}
	if got := waitFor(t, done, "secretFor"); got.tok != "access-1" || !got.ok {
		t.Fatalf("secretFor = %q, ok=%v; want access-1 with no error", got.tok, got.ok)
	}
	if creds, err := s.OAuth(c.ID); err != nil || creds.RefreshToken != "rotated-1" {
		t.Fatalf("stored refresh token = %q (%v), want rotated-1", creds.RefreshToken, err)
	}
	if sec, _ := s.Secret(c.ID); sec != "access-1" {
		t.Errorf("stored secret = %q, want access-1", sec)
	}

	// forceRefresh: the same, on the 401 recovery path.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan result, 1)
	go func() {
		tok, ok := a.forceRefresh(ctx2, c.ID)
		done2 <- result{tok, ok}
	}()
	waitFor(t, arrived, "the token endpoint to receive the forceRefresh exchange")
	cancel2()
	release <- struct{}{}
	if got := waitFor(t, done2, "forceRefresh"); got.tok != "access-2" || !got.ok {
		t.Fatalf("forceRefresh = %q, ok=%v; want access-2 and true", got.tok, got.ok)
	}
	if creds, err := s.OAuth(c.ID); err != nil || creds.RefreshToken != "rotated-2" {
		t.Fatalf("stored refresh token = %q (%v), want rotated-2", creds.RefreshToken, err)
	}
}

// The exchange succeeds and the write fails. The new token is the only one the
// provider accepts, so the caller must get it, and the operator must hear that
// it was not saved. T1-5 (b).
func TestRefreshReturnsTheNewTokenWhenTheStoreFails(t *testing.T) {
	cases := []struct {
		name string
		call func(*api, string) (string, bool)
	}{
		{"secretFor", func(a *api, id string) (string, bool) {
			tok, err := a.secretFor(context.Background(), id)
			return tok, err == nil
		}},
		{"forceRefresh", func(a *api, id string) (string, bool) {
			return a.forceRefresh(context.Background(), id)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowDial(t)
			alerts := make(chan notifyMsg, 4)
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m notifyMsg
				json.NewDecoder(r.Body).Decode(&m)
				alerts <- m
			}))
			defer sink.Close()

			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			c, _ := s.CreateConnection("claude", "P", "stale-token")
			// The write fails for a reason the code cannot foresee: the row is
			// unreachable by the time the provider answers.
			var once sync.Once
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { s.DB.Exec("DROP TABLE connections") })
				w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"rotated","expires_in":3600}`))
			}))
			defer tokenSrv.Close()
			s.SetOAuth(c.ID, store.OAuthCreds{RefreshToken: "rt-0", TokenURL: tokenSrv.URL, ClientID: "cid",
				ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
			if _, err := s.SaveNotifyChannel(store.NotifyChannel{Name: "hook", Type: "webhook", Enabled: true,
				Config: map[string]string{"url": loopbackName(t, sink.URL)}}); err != nil {
				t.Fatalf("save channel: %v", err)
			}
			a, _ := newServer(s, nil, nil)

			tok, ok := tc.call(a, c.ID)
			if tok != "fresh-token" || !ok {
				t.Fatalf("%s = %q, ok=%v; want fresh-token and no failure", tc.name, tok, ok)
			}
			m := waitFor(t, alerts, "the alert about the unsaved token")
			text := m.Title + " " + strings.Join(m.Lines, " ")
			if m.Event != EventAccountAuth {
				t.Errorf("event = %q, want %q", m.Event, EventAccountAuth)
			}
			if !strings.Contains(text, c.ID) {
				t.Errorf("alert %q does not name connection %s", text, c.ID)
			}
			if !strings.Contains(text, "not saved") {
				t.Errorf("alert %q does not say the new token was not saved", text)
			}
		})
	}
}

// Refresh must use the body encoding of the provider's sign-in exchange. A
// provider whose token endpoint takes JSON refuses a form, so every refresh
// after the first expiry fails. T1-6.
func TestRefreshUsesTheSignInBodyEncoding(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		wantJSON bool
		secret   string
		// declare adds a provider def before the connection is made.
		declare *provider.Def
	}{
		{name: "built-in json token endpoint", provider: "claude", wantJSON: true, secret: "csec"},
		{name: "built-in form token endpoint", provider: "codex"},
		{name: "declared device provider with jsonToken", provider: "acme", wantJSON: true, secret: "csec",
			declare: &provider.Def{ID: "acme", Kind: provider.KindOAuthDevice, API: "openai",
				BaseURL: "https://acme.example.com/v1", ModelsURL: "none", Models: []string{"m1"},
				OAuth: &provider.OAuth{DeviceCodeURL: "https://acme.example.com/device",
					TokenURL: "https://acme.example.com/token", ClientID: "cid", JSONToken: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				fields := map[string]string{}
				isJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
				if isJSON != tc.wantJSON {
					http.Error(w, `{"error":"this endpoint refuses that body"}`, http.StatusBadRequest)
					return
				}
				if isJSON {
					json.Unmarshal(body, &fields)
				} else {
					v, _ := url.ParseQuery(string(body))
					for k := range v {
						fields[k] = v.Get(k)
					}
				}
				if fields["grant_type"] != "refresh_token" || fields["refresh_token"] == "" || fields["client_id"] != "cid" {
					http.Error(w, `{"error":"missing fields"}`, http.StatusBadRequest)
					return
				}
				if fields["client_secret"] != tc.secret {
					http.Error(w, `{"error":"client_secret"}`, http.StatusBadRequest)
					return
				}
				i := atomic.AddInt32(&n, 1)
				fmt.Fprintf(w, `{"access_token":"tok-%d","refresh_token":"rt-%d","expires_in":3600}`, i, i)
			}))
			defer tokenSrv.Close()

			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			if tc.declare != nil {
				if err := s.PutProviderDef(*tc.declare); err != nil {
					t.Fatalf("declare provider: %v", err)
				}
			}
			c, _ := s.CreateConnection(tc.provider, "P", "stale-token")
			s.SetOAuth(c.ID, store.OAuthCreds{RefreshToken: "rt-0", TokenURL: tokenSrv.URL, ClientID: "cid",
				ClientSecret: tc.secret, ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
			a, _ := newServer(s, nil, nil)

			tok, err := a.secretFor(context.Background(), c.ID)
			if err != nil || tok != "tok-1" {
				t.Fatalf("secretFor = %q, %v; want tok-1", tok, err)
			}
			if creds, _ := s.OAuth(c.ID); creds.RefreshToken != "rt-1" {
				t.Errorf("stored refresh token = %q, want rt-1", creds.RefreshToken)
			}
			tok2, ok := a.forceRefresh(context.Background(), c.ID)
			if !ok || tok2 != "tok-2" {
				t.Fatalf("forceRefresh = %q, ok=%v; want tok-2 and true", tok2, ok)
			}
		})
	}
}

func TestOAuthUpdateAfterRefreshFailureRetainsInMemory(t *testing.T) {
	var callCount int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&callCount, 1)
		body, _ := io.ReadAll(r.Body)
		var rt string
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var fields map[string]string
			json.Unmarshal(body, &fields)
			rt = fields["refresh_token"]
		} else {
			vals, _ := url.ParseQuery(string(body))
			rt = vals.Get("refresh_token")
		}
		if cnt == 1 {
			if rt != "rt-0" {
				http.Error(w, `{"error":"invalid_grant","error_description":"expected rt-0"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok-1","refresh_token":"rt-1","expires_in":3600}`))
			return
		}
		if cnt == 2 {
			if rt != "rt-1" {
				http.Error(w, `{"error":"invalid_grant","error_description":"old refresh token refused"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok-2","refresh_token":"rt-2","expires_in":3600}`))
			return
		}
		http.Error(w, `{"error":"unexpected call"}`, http.StatusBadRequest)
	}))
	defer tokenSrv.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()

	c, _ := s.CreateConnection("claude", "P", "stale-token")
	s.SetOAuth(c.ID, store.OAuthCreds{
		RefreshToken: "rt-0",
		TokenURL:     tokenSrv.URL,
		ClientID:     "cid",
		ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})

	_, err := s.DB.Exec("CREATE TRIGGER fail_once BEFORE UPDATE OF refresh_token ON connections BEGIN SELECT RAISE(FAIL, 'disk write failure'); END;")
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	a, _ := newServer(s, nil, nil)

	tok1, err := a.secretFor(context.Background(), c.ID)
	if err != nil || tok1 != "tok-1" {
		t.Fatalf("first secretFor = %q, %v; want tok-1", tok1, err)
	}
	if calls := atomic.LoadInt32(&callCount); calls != 1 {
		t.Fatalf("token server calls after first secretFor = %d, want 1", calls)
	}

	tok2, err := a.secretFor(context.Background(), c.ID)
	if err != nil || tok2 != "tok-1" {
		t.Fatalf("second secretFor = %q, %v; want tok-1", tok2, err)
	}
	if calls := atomic.LoadInt32(&callCount); calls != 1 {
		t.Fatalf("token server calls after second secretFor = %d, want 1 (should not hit token server again)", calls)
	}

	_, err = s.DB.Exec("DROP TRIGGER fail_once")
	if err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	tok3, ok := a.forceRefresh(context.Background(), c.ID)
	if !ok || tok3 != "tok-2" {
		t.Fatalf("forceRefresh = %q, ok=%v; want tok-2 and true", tok3, ok)
	}
	if calls := atomic.LoadInt32(&callCount); calls != 2 {
		t.Fatalf("token server calls after forceRefresh = %d, want 2", calls)
	}

	creds, err := s.OAuth(c.ID)
	if err != nil {
		t.Fatalf("s.OAuth: %v", err)
	}
	if creds.RefreshToken != "rt-2" {
		t.Fatalf("persisted refresh token = %q, want rt-2", creds.RefreshToken)
	}
}
