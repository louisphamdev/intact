package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// Anthropic refuses the 1M-context beta to a model with a smaller window ("The
// long context beta is not yet available for this subscription"). intact sends
// its default beta list to every model, so the flag is dropped where the
// provider's model list gives a smaller window, and kept where it is 1M or
// unknown. Claude Code itself (Bifrost) keeps the list it sent.
func TestLongContextBetaFollowsTheModelWindow(t *testing.T) {
	var betas []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betas = append(betas, r.Header.Get("Anthropic-Beta"))
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "a", "acct-a")
	a, h := newServer(s, map[string]string{"claude": up.URL}, nil)
	a.cat.m["claude"] = catalogEntry{ok: true, ids: []string{"small", "big", "unknown"},
		info: map[string]ModelInfo{"small": {Input: 200000}, "big": {Input: 1000000}}}
	send := func(model, ua, beta string) string {
		r := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/`+model+`","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Content-Type", "application/json")
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if beta != "" {
			r.Header.Set("Anthropic-Beta", beta)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		return betas[len(betas)-1]
	}
	for _, c := range []struct {
		name, model, ua, beta string
		want                  bool
	}{
		{"small window", "small", "", "", false},
		{"1M window", "big", "", "", true},
		{"unknown window", "unknown", "", "", true},
		{"Claude Code keeps its list", "small", "claude-cli/2.1.287 (external, cli)", "claude-code-20250219,oauth-2025-04-20,context-1m-2025-08-07", true},
	} {
		got := send(c.model, c.ua, c.beta)
		if has := strings.Contains(got, "context-1m-"); has != c.want {
			t.Errorf("%s: Anthropic-Beta = %q, context-1m present %v, want %v", c.name, got, has, c.want)
		}
		if !strings.Contains(got, "oauth-2025-04-20") {
			t.Errorf("%s: the credential betas went missing: %q", c.name, got)
		}
	}
}
