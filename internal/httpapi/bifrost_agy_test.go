package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

const agyCLIUA = "antigravity/cli/1.2.14 (aidev_client; os_type=windows; arch=amd64; cl=990662481; auth_method=consumer)"

// agyBody is a Code Assist request as the Antigravity CLI writes it, with the
// model named the way llm-switcher sends it to intact.
const agyBody = `{"project":"caller-project","requestId":"agent/a/1/t/1","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"labels":{"trajectory_id":"t-1"},"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}},"sessionId":"-1"},"model":"antigravity/gemini-3.8-flash-high","userAgent":"antigravity","requestType":"agent"}`

const agySSE = `data: {"response": {"candidates": [{"content": {"role": "model","parts": [{"text": "OK"}]},"finishReason": "STOP"}],"usageMetadata": {"promptTokenCount": 4446,"candidatesTokenCount": 29,"totalTokenCount": 4500,"thoughtsTokenCount": 25,"cachedContentTokenCount": 4425},"modelVersion": "gemini-3.8-flash"},"traceId": "t"}` + "\n\n"

type agyUpstream struct {
	mu                 sync.Mutex
	path, ua, auth, ae string
	body               string
	accounts           []string
}

func agyBifrostServer(t *testing.T) (*httptest.Server, *agyUpstream) {
	t.Helper()
	got := &agyUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			project := "pool-project"
			if r.Header.Get("Authorization") == "Bearer token-b" {
				project = "pool-project-b"
			}
			w.Write([]byte(`{"cloudaicompanionProject":"` + project + `"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			w.Write([]byte(agModelsFixture))
		default:
			got.mu.Lock()
			got.path, got.ua, got.auth, got.body = r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("User-Agent"), r.Header.Get("Authorization"), string(b)
			got.accounts = append(got.accounts, r.Header.Get("Authorization"))
			got.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, agySSE)
		}
	}))
	t.Cleanup(up.Close)
	return up, got
}

// The Antigravity CLI reaching an Antigravity account arrives as the CLI: the
// body and headers pass, only the credential and the account's project change.
func TestBifrostAntigravityChangesOnlyTheCredential(t *testing.T) {
	up, got := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("antigravity", "pool", "token-a")
	h := New(s, map[string]string{"antigravity": up.URL})

	req := loopbackRequest("POST", "/v1/v1internal:streamGenerateContent?alt=sse", strings.NewReader(agyBody))
	req.Header.Set("User-Agent", agyCLIUA)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.path != "/v1internal:streamGenerateContent?alt=sse" {
		t.Errorf("upstream path = %s", got.path)
	}
	if got.ua != agyCLIUA || got.auth != "Bearer token-a" {
		t.Errorf("User-Agent = %q, Authorization = %q", got.ua, got.auth)
	}
	want := strings.NewReplacer(`"caller-project"`, `"pool-project"`, `"antigravity/gemini-3.8-flash-high"`, `"gemini-3.8-flash-high"`).Replace(agyBody)
	if got.body != want {
		t.Errorf("body changed beyond project and model:\n got %s\nwant %s", got.body, want)
	}
	if !strings.Contains(rec.Body.String(), `"usageMetadata"`) || !strings.Contains(rec.Body.String(), `"traceId"`) {
		t.Errorf("the CLI must get Code Assist events back unchanged: %s", rec.Body.String())
	}
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].ConnectionID != c.ID || rows[0].InputTokens != 4446 || rows[0].OutputTokens != 54 || rows[0].CachedTokens != 4425 {
		t.Errorf("usage = %+v, want input 4446, output 29+25, cached 4425", rows)
	}
}

// The non-streaming call of the CLI keeps its own path and gets no query added.
func TestBifrostAntigravityGenerateContentKeepsItsPath(t *testing.T) {
	up, got := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "pool", "token-a")
	h := New(s, map[string]string{"antigravity": up.URL})
	req := loopbackRequest("POST", "/v1/v1internal:generateContent", strings.NewReader(agyBody))
	req.Header.Set("User-Agent", agyCLIUA)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.path != "/v1internal:generateContent?" {
		t.Errorf("upstream path = %s", got.path)
	}
}

// One CLI conversation stays on one account, so its prompt cache is reused.
func TestBifrostAntigravityPinsATrajectoryToAnAccount(t *testing.T) {
	up, got := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "a", "token-a")
	s.CreateConnection("antigravity", "b", "token-b")
	s.SetSetting("rotation:antigravity", `{"mode":"round-robin","sticky":1}`)
	h := New(s, map[string]string{"antigravity": up.URL})
	for i := 0; i < 3; i++ {
		req := loopbackRequest("POST", "/v1/v1internal:streamGenerateContent?alt=sse", strings.NewReader(agyBody))
		req.Header.Set("User-Agent", agyCLIUA)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if len(got.accounts) != 3 || got.accounts[0] != got.accounts[1] || got.accounts[1] != got.accounts[2] {
		t.Errorf("one trajectory moved across accounts: %v", got.accounts)
	}
}

// llm-switcher learns from the model entry that the CLI may cross unchanged.
func TestModelsNameTheAntigravityClient(t *testing.T) {
	up, _ := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "pool", "token-a")
	h := New(s, map[string]string{"antigravity": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/antigravity/gemini-3.8-flash", nil))
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e["bifrost_ua"] != "antigravity/cli/" {
		t.Errorf("bifrost_ua = %v (body %s)", e["bifrost_ua"], rec.Body.String())
	}
}

// The Codex CLI reaching a Codex account keeps its own identity and body.
func TestBifrostCodexChangesOnlyTheCredential(t *testing.T) {
	var gotUA, gotBody, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "responses") {
			gotUA, gotBody, gotAuth = r.Header.Get("User-Agent"), string(b), r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("codex", "pool", "codex-token")
	h := New(s, map[string]string{"codex": up.URL})
	body := `{"model":"codex/gpt-5.5","instructions":"x","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true,"store":false}`
	req := loopbackRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("User-Agent", "codex_cli_rs/0.160.0 (Windows 10.0.26300; x86_64) WindowsTerminal")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotUA != "codex_cli_rs/0.160.0 (Windows 10.0.26300; x86_64) WindowsTerminal" {
		t.Errorf("User-Agent = %q: the CLI's own identity must pass", gotUA)
	}
	if gotAuth != "Bearer codex-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if want := strings.Replace(body, "codex/gpt-5.5", "gpt-5.5", 1); gotBody != want {
		t.Errorf("body changed:\n got %s\nwant %s", gotBody, want)
	}
}

// The CLI names a level variant (gemini-3.8-flash-high); intact lists the folded
// base. The lookup must still answer, or llm-switcher never turns Bifrost on.
func TestModelLookupFindsALevelVariant(t *testing.T) {
	up, _ := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "pool", "token-a")
	h := New(s, map[string]string{"antigravity": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/antigravity/gemini-3.8-flash-high", nil))
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if e["id"] != "antigravity/gemini-3.8-flash-high" || e["bifrost_ua"] != "antigravity/cli/" {
		t.Errorf("entry = %s", rec.Body.String())
	}
}
