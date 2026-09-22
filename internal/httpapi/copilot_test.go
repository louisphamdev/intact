package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestCopilotExchangesAndCachesTheToken(t *testing.T) {
	var exchanges int32
	ex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchanges, 1)
		if r.Header.Get("Authorization") != "token gho_abc" {
			t.Errorf("exchange auth = %q", r.Header.Get("Authorization"))
		}
		exp := time.Now().Add(30 * time.Minute).Unix()
		w.Write([]byte(`{"token":"cop-1","expires_at":` + itoa(exp) + `}`))
	}))
	defer ex.Close()
	old := copilotTokenURL
	copilotTokenURL = ex.URL
	defer func() { copilotTokenURL = old }()

	var gotAuth, gotIntegration, gotReqID, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotIntegration, gotReqID, gotBody = r.Header.Get("Authorization"), r.Header.Get("Copilot-Integration-Id"), r.Header.Get("X-Request-Id"), string(b)
		w.Write([]byte(`{"ok":true,"copilot_extra":1}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("github", "gh", "gho_abc")
	h := New(s, map[string]string{"github": up.URL})

	for i := 0; i < 2; i++ {
		rec := postV1(h, `{"model":"github/gpt-5.4","max_tokens":10,"messages":[]}`)
		if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true,"copilot_extra":1}` {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if gotAuth != "Bearer cop-1" || gotIntegration != "vscode-chat" || len(gotReqID) != 36 {
		t.Errorf("auth=%q integration=%q request id=%q", gotAuth, gotIntegration, gotReqID)
	}
	if !strings.Contains(gotBody, `"max_completion_tokens":10`) || strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf("body = %s, want max_tokens renamed for gpt-5", gotBody)
	}
	if n := atomic.LoadInt32(&exchanges); n != 1 {
		t.Errorf("exchanges = %d, want 1 (cached)", n)
	}
}

func itoa(n int64) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
