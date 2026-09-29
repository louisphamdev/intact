package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestAPIKeyExpiryRateLimitAndUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := NewWithAuth(s, map[string]string{"groq": up.URL}, authConfig())
	admin := New(s, map[string]string{"groq": up.URL})
	k, _ := s.CreateAPIKey("laptop", nil)
	other, _ := s.CreateAPIKey("other", nil)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		var rec = httptest.NewRecorder()
		req := loopbackRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		admin.ServeHTTP(rec, req)
		return rec
	}
	limits := func(body string) int { return send("POST", "/keys/"+k.ID+"/limits", body).Code }
	call := func(tok string) *httptest.ResponseRecorder {
		return sendAs(h, "POST", "/v1/chat/completions", `{"model":"groq/m","messages":[]}`, tok)
	}

	for _, bad := range []string{`{"rpm":-1}`, `{"expiresAt":"tomorrow"}`, `{"rpm":"5"}`} {
		if code := limits(bad); code != http.StatusBadRequest {
			t.Errorf("limits %s: code=%d, want 400", bad, code)
		}
	}

	// Two requests a minute: the third is refused with a wait, another key is not.
	if code := limits(`{"rpm":2}`); code != 200 {
		t.Fatalf("set rpm: %d", code)
	}
	for i := 0; i < 2; i++ {
		if rec := call(k.Key); rec.Code != 200 {
			t.Fatalf("call %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := call(k.Key)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("over the rate: %d %s", rec.Code, rec.Body.String())
	}
	if s, _ := strconv.Atoi(rec.Header().Get("Retry-After")); s < 1 || s > 60 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if rec := call(other.Key); rec.Code != 200 {
		t.Errorf("another key was limited: %d", rec.Code)
	}

	// Usage is kept per key: the two calls that went through, not the refused one.
	var u struct {
		Rows []store.KeyUsageRow `json:"rows"`
	}
	json.Unmarshal(send("GET", "/keys/"+k.ID+"/usage", "").Body.Bytes(), &u)
	if len(u.Rows) != 1 || u.Rows[0].Requests != 2 || u.Rows[0].InputTokens != 10 || u.Rows[0].OutputTokens != 4 || u.Rows[0].Model != "m" {
		t.Errorf("key usage = %+v", u.Rows)
	}
	var listed struct{ Keys []store.APIKey }
	json.Unmarshal(send("GET", "/keys", "").Body.Bytes(), &listed)
	found := false
	for _, x := range listed.Keys {
		if x.ID == k.ID {
			found = true
			if x.RPM != 2 || x.LastUsed == "" {
				t.Errorf("listed key = %+v", x)
			}
		}
	}
	if !found {
		t.Errorf("key not in the listing: %+v", listed.Keys)
	}

	// An expired key is refused; a future expiry and no expiry both work.
	if code := limits(`{"expiresAt":"2020-01-01T00:00:00Z","rpm":0}`); code != 200 {
		t.Fatalf("set expiry: %d", code)
	}
	if rec := call(k.Key); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("expired key: %d %s", rec.Code, rec.Body.String())
	}
	limits(`{"expiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)
	if rec := call(k.Key); rec.Code != 200 {
		t.Errorf("key with a future expiry: %d %s", rec.Code, rec.Body.String())
	}
	limits(`{"expiresAt":""}`)
	if rec := call(k.Key); rec.Code != 200 {
		t.Errorf("key with no expiry: %d", rec.Code)
	}
}
