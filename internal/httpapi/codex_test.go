package httpapi

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func fakeJWT(accountID string) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `"}}`))
	return "h." + p + ".s"
}

func TestCodexTranslatesChatAndPassesResponsesThrough(t *testing.T) {
	var gotPath, gotAccount, gotOriginator, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAccount, gotOriginator, gotBody = r.URL.Path, r.Header.Get("ChatGPT-Account-ID"), r.Header.Get("Originator"), string(b)
		// The real backend labels its event stream application/json.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1},\"extra\":\"kept\"}}\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "cx", fakeJWT("acct-9"))
	h := New(s, map[string]string{"codex": up.URL})

	rec := postV1(h, `{"model":"codex/gpt-5.5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if gotPath != "/responses" || gotAccount != "acct-9" || gotOriginator != "codex_cli_rs" || !strings.Contains(gotBody, `"instructions":"sys"`) {
		t.Errorf("upstream got path=%s account=%s originator=%s body=%s", gotPath, gotAccount, gotOriginator, gotBody)
	}
	if b := rec.Body.String(); !strings.Contains(b, `"object":"chat.completion"`) || !strings.Contains(b, `"content":"OK"`) {
		t.Errorf("chat caller got %s", b)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"codex/gpt-5.5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if b := rec.Body.String(); !strings.Contains(b, "event: message_start") || !strings.Contains(b, `"text":"OK"`) {
		t.Errorf("anthropic caller got %s", b)
	}

	// A Responses caller gets the provider's bytes, every field included.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"codex/gpt-5.5","input":"hi","stream":true}`)))
	if b := rec.Body.String(); !strings.Contains(b, `"extra":"kept"`) || gotBody != `{"model":"gpt-5.5","input":"hi","stream":true}` {
		t.Errorf("passthrough: body sent %s, got %s", gotBody, b)
	}
}
