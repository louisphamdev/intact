package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestGroupVariants(t *testing.T) {
	bases, groups := groupVariants([]string{
		"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-low", "gemini-3.8-flash-tiered",
		"gemini-3.1-pro-high", "gemini-3.1-pro-low",
		"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3.5-flash-lite",
		"gemini-3-flash", "claude-sonnet-4-6", "gpt-oss-120b-medium",
	})
	want := []string{"claude-sonnet-4-6", "gemini-3-flash", "gemini-3.1-pro", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.8-flash", "gpt-oss-120b"}
	if !reflect.DeepEqual(bases, want) {
		t.Fatalf("bases = %v", bases)
	}
	g := groups["gemini-3.8-flash"]
	if g.def != "tiered" || !reflect.DeepEqual(g.Variants(), []string{"tiered", "high", "medium", "low"}) {
		t.Errorf("3.8 flash = %q %v", g.def, g.Variants())
	}
	if groups["gemini-3.1-pro"].def != "high" || groups["gemini-3.5-flash"].def != "low" || groups["gpt-oss-120b"].def != "medium" {
		t.Errorf("defaults: %+v", groups)
	}
	if _, ok := groups["gemini-3-flash"]; ok {
		t.Error("a plain model was folded")
	}
	for effort, id := range map[string]string{
		"": "gemini-3.8-flash-tiered", "none": "gemini-3.8-flash-tiered", "high": "gemini-3.8-flash-high",
		"xhigh": "gemini-3.8-flash-high", "medium": "gemini-3.8-flash-medium", "minimal": "gemini-3.8-flash-low",
	} {
		if got := g.pick(effort); got != id {
			t.Errorf("pick(%q) = %s, want %s", effort, got, id)
		}
	}
	// Nearest level: pro has high and low; medium ties and takes high.
	if got := groups["gemini-3.1-pro"].pick("medium"); got != "gemini-3.1-pro-high" {
		t.Errorf("pro medium = %s", got)
	}
}

func TestEffortOf(t *testing.T) {
	for body, want := range map[string]string{
		`{"reasoning_effort":"High"}`:                                      "high",
		`{"reasoning":{"effort":"low"}}`:                                   "low",
		`{"output_config":{"effort":"medium"}}`:                            "medium",
		`{"thinking":{"type":"enabled","budget_tokens":1024}}`:             "low",
		`{"thinking":{"type":"enabled","budget_tokens":32000}}`:            "high",
		`{"thinking":{"type":"disabled"}}`:                                 "none",
		`{"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`:  "medium",
		`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`: "high",
		`{"messages":[]}`: "",
	} {
		if got := effortOf([]byte(body)); got != want {
			t.Errorf("effortOf(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestAntigravityVariantsFoldAndFallBack(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	busy := map[string]bool{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Write([]byte(`{"cloudaicompanionProject":"p"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			w.Write([]byte(`{"models":{"gemini-3.8-flash-high":{},"gemini-3.8-flash-low":{},"gemini-3.8-flash-tiered":{},"gemini-3-flash":{},"gemini-3.1-pro-high":{},"gemini-3.1-pro-low":{}},"deprecatedModelIds":{"gemini-3.1-pro-high":{"newModelId":"gemini-pro-agent"}}}`))
		default:
			var env struct{ Model string }
			json.Unmarshal(b, &env)
			mu.Lock()
			sent = append(sent, env.Model)
			refuse := busy[env.Model]
			mu.Unlock()
			if refuse {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":{"code":429,"message":"capacity exhausted"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"responseId":"r","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`+"\n\n")
		}
	}))
	defer up.Close()
	old := antigravityProdURL
	antigravityProdURL = up.URL
	defer func() { antigravityProdURL = old }()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "a1", "ya29.a")
	s.CreateConnection("antigravity", "a2", "ya29.b")
	h := New(s, map[string]string{"antigravity": up.URL})

	// One model per base in the list, with its variants beside it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if b := rec.Body.String(); !strings.Contains(b, `"antigravity/gemini-3.8-flash"`) || strings.Contains(b, "flash-high") {
		t.Errorf("models = %s", b)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/antigravity/model-table", nil))
	if !strings.Contains(rec.Body.String(), `"gemini-3.8-flash":["tiered","high","low"]`) || !strings.Contains(rec.Body.String(), `"gemini-3.1-pro":["low"]`) {
		t.Errorf("table variants = %s", rec.Body.String())
	}
	call := func(body string) (*httptest.ResponseRecorder, []string) {
		mu.Lock()
		sent = nil
		mu.Unlock()
		rec := postV1(h, body)
		mu.Lock()
		defer mu.Unlock()
		return rec, append([]string(nil), sent...)
	}
	// No effort: the tiered variant. An effort: its level. A bare base id too.
	if _, got := call(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`); len(got) != 1 || got[0] != "gemini-3.8-flash-tiered" {
		t.Errorf("no effort sent %v", got)
	}
	if _, got := call(`{"model":"gemini-3.8-flash","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`); len(got) != 1 || got[0] != "gemini-3.8-flash-high" {
		t.Errorf("high sent %v", got)
	}
	// The full id still works, as a bare id and with the prefix.
	if rec, got := call(`{"model":"gemini-3.8-flash-low","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 || got[0] != "gemini-3.8-flash-low" {
		t.Errorf("full id: %d %v", rec.Code, got)
	}
	// -high busy on every account: each account tries it, then the default.
	busy["gemini-3.8-flash-high"] = true
	rec, got := call(`{"model":"antigravity/gemini-3.8-flash","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !reflect.DeepEqual(got, []string{"gemini-3.8-flash-high", "gemini-3.8-flash-high", "gemini-3.8-flash-tiered"}) {
		t.Errorf("fallback: %d %v", rec.Code, got)
	}
	if rec.Header().Get("X-Intact-Model") != "gemini-3.8-flash-tiered" {
		t.Errorf("X-Intact-Model = %q", rec.Header().Get("X-Intact-Model"))
	}
	// Switching the base off refuses its variants too.
	s.SetModelsActive("antigravity", []string{"gemini-3.8-flash"}, false)
	if rec, _ := call(`{"model":"antigravity/gemini-3.8-flash-high","messages":[]}`); rec.Code != http.StatusForbidden {
		t.Errorf("off base, variant id: %d", rec.Code)
	}
}

func TestModelInfos(t *testing.T) {
	cases := map[string]string{
		"anthropic":   `{"data":[{"id":"c","capabilities":{"thinking":{"supported":true},"effort":{"supported":true,"low":{"supported":true},"max":{"supported":true},"xhigh":{"supported":false}}}}]}`,
		"codex":       `{"models":[{"slug":"g","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"default_reasoning_level":"low"}]}`,
		"copilot":     `{"data":[{"id":"a","capabilities":{"supports":{"reasoning_effort":["high","low"]}}},{"id":"b","capabilities":{"supports":{"tool_calls":true}}},{"id":"h","capabilities":{"supports":{"max_thinking_budget":32000}}}]}`,
		"openrouter":  `{"data":[{"id":"r","supported_parameters":["reasoning","reasoning_effort"],"reasoning":{"mandatory":true}},{"id":"n","supported_parameters":["tools"]}]}`,
		"groq":        `{"data":[{"id":"q","supported_features":["tools","reasoning"]},{"id":"l","supported_features":["json_mode"]},{"id":"w"}]}`,
		"antigravity": `{"models":{"x-high":{"supportsThinking":true},"x-low":{"supportsThinking":true},"img":{}}}`,
		"nvidia":      `{"data":[{"id":"m","owned_by":"x"}]}`,
		"cloudflare":  `{"result":[{"id":"0b1c-uuid","name":"@cf/a","properties":[{"property_id":"reasoning","value":"true"},{"property_id":"reasoning_effort","value":{"supported_efforts":["high","low"],"default_effort":"low","mandatory":true}},{"property_id":"require_workers_paid","value":"true"}]},{"name":"@cf/b","properties":[{"property_id":"context_window","value":"8000"}]}]}`,
	}
	got := map[string]string{}
	for name, raw := range cases {
		infos := modelInfos([]byte(raw))
		if name == "antigravity" {
			_, g := groupVariants([]string{"x-high", "x-low", "img"})
			infos = foldInfos(infos, g)
		}
		b, _ := json.Marshal(infos)
		got[name] = string(b)
	}
	want := map[string]string{
		"anthropic":   `{"c":{"thinking":true,"efforts":["low","max"]}}`,
		"codex":       `{"g":{"thinking":true,"efforts":["low","high"],"default":"low"}}`,
		"copilot":     `{"a":{"thinking":true,"efforts":["low","high"]},"b":{"thinking":false},"h":{"thinking":true}}`,
		"openrouter":  `{"n":{"thinking":false},"r":{"thinking":true,"always":true,"efforts":["low","medium","high"]}}`,
		"groq":        `{"l":{"thinking":false},"q":{"thinking":true},"w":{"thinking":false}}`,
		"antigravity": `{"img":{"thinking":false},"x":{"thinking":true,"efforts":["high","low"],"default":"high"}}`,
		"nvidia":      `null`,
		"cloudflare":  `{"@cf/a":{"thinking":true,"always":true,"efforts":["low","high"],"default":"low","paid":true},"@cf/b":{"thinking":false}}`,
	}
	for k := range cases {
		if got[k] != want[k] {
			t.Errorf("%s:\n got %s\nwant %s", k, got[k], want[k])
		}
	}
}

func TestCloudflareListsFromModelSearch(t *testing.T) {
	var gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{"success":true,"result":[{"name":"@cf/x/new","properties":[{"property_id":"reasoning","value":"true"}]}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("cloudflare-ai", "cf", "k")
	s.SetBaseURL(c.ID, up.URL+"/client/v4/accounts/acc/ai/v1")
	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/cloudflare-ai/model-table", nil))
	if gotPath != "/client/v4/accounts/acc/ai/models/search" || !strings.Contains(gotQuery, "task=Text%20Generation") {
		t.Errorf("list read at %s?%s", gotPath, gotQuery)
	}
	if b := rec.Body.String(); !strings.Contains(b, `"model":"@cf/x/new"`) || strings.Contains(b, "llama-3.2-1b") || !strings.Contains(b, `"thinking":true`) {
		t.Errorf("table = %s", b)
	}
}
