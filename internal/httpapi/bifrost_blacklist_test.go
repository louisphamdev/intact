package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/store"
)

// saveRules stores enabled blacklist rules for one provider and returns them compiled,
// so a test can prove each rule would change the body it then expects to pass unchanged.
func saveRules(t *testing.T, s *store.Store, prov string, rules [][2]string) []filter.Rule {
	t.Helper()
	var out []filter.Rule
	for _, r := range rules {
		if _, err := s.SaveFilter(store.Filter{Provider: prov, Kind: r[0], Pattern: r[1], Enabled: true}); err != nil {
			t.Fatal(err)
		}
		c, err := filter.Compile(r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// The Antigravity CLI crossing Bifrost gets no blacklist, even with rules that hit its body and headers.
func TestBifrostAntigravitySkipsTheBlacklist(t *testing.T) {
	var gotBody, gotProbe string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Write([]byte(`{"cloudaicompanionProject":"pool-project"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			w.Write([]byte(agModelsFixture))
		default:
			gotBody, gotProbe = string(b), r.Header.Get("X-Probe")
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, agySSE)
		}
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "pool", "token-a")

	body := `{"project":"caller-project","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"role":"user","parts":[{"text":"You are a Claude agent, built on the SDK."}]},"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","$id":"x","properties":{}}}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}},"model":"antigravity/gemini-3.8-flash-high","userAgent":"antigravity","requestType":"agent"}`
	rules := saveRules(t, s, "antigravity", [][2]string{
		{"field", "request.generationConfig.thinkingConfig"},
		{"system", `\ba\s+Claude\s+agent,`},
		{"schema", "$id"},
		{"header", "x-probe"},
	})
	if _, changed := filter.Apply([]byte(body), rules); !changed {
		t.Fatal("the rules do not touch the body, so the test proves nothing")
	}

	h := New(s, map[string]string{"antigravity": up.URL})
	req := loopbackRequest("POST", "/v1/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
	req.Header.Set("User-Agent", agyCLIUA)
	req.Header.Set("X-Probe", "kept")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := strings.NewReplacer(`"caller-project"`, `"pool-project"`, `"antigravity/gemini-3.8-flash-high"`, `"gemini-3.8-flash-high"`).Replace(body)
	if gotBody != want {
		t.Errorf("the blacklist ran on a Bifrost body:\n got %s\nwant %s", gotBody, want)
	}
	if gotProbe != "kept" {
		t.Errorf("X-Probe = %q: a header rule ran on a Bifrost request", gotProbe)
	}
}

// The Codex CLI crossing Bifrost gets no blacklist; the same request from another client does.
func TestBifrostCodexSkipsTheBlacklist(t *testing.T) {
	var gotBody, gotProbe string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "responses") {
			gotBody, gotProbe = string(b), r.Header.Get("X-Probe")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "pool", "codex-token")
	saveRules(t, s, "codex", [][2]string{
		{"field", "reasoning"},
		{"system", `\bYou\s+are\s+Codex,\s+a\b`},
		{"header", "x-probe"},
	})
	h := New(s, map[string]string{"codex": up.URL})
	body := `{"model":"codex/gpt-5.5","instructions":"You are Codex, a coding agent.","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true,"store":false}`

	send := func(ua string) {
		req := loopbackRequest("POST", "/v1/responses", strings.NewReader(body))
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-Probe", "kept")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	send("codex_cli_rs/0.160.0 (Windows 10.0.26300; x86_64) WindowsTerminal")
	if want := strings.Replace(body, "codex/gpt-5.5", "gpt-5.5", 1); gotBody != want {
		t.Errorf("the blacklist ran on a Bifrost body:\n got %s\nwant %s", gotBody, want)
	}
	if gotProbe != "kept" {
		t.Errorf("X-Probe = %q: a header rule ran on a Bifrost request", gotProbe)
	}

	send("opencode/1.0")
	if strings.Contains(gotBody, `"reasoning"`) || strings.Contains(gotBody, "You are Codex, a") || gotProbe != "" {
		t.Errorf("another client must still get the blacklist: body %s, X-Probe %q", gotBody, gotProbe)
	}
}
