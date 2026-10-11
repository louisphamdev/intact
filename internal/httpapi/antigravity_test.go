package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestAntigravityWrapsAndTranslates(t *testing.T) {
	var gotPath, gotUA, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Write([]byte(`{"cloudaicompanionProject":"proj-7"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			w.Write([]byte(`{"models":{"gemini-3-flash":{},"internal-x":{"isInternal":true}}}`))
		default:
			gotPath, gotUA, gotBody = r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("User-Agent"), string(b)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"responseId":"r","modelVersion":"gemini-3-flash-n","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}}`+"\n\n")
		}
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("antigravity", "ag", "fake-access-token")
	h := New(s, map[string]string{"antigravity": up.URL})

	rec := postV1(h, `{"model":"antigravity/gemini-3-flash","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if b := rec.Body.String(); !strings.Contains(b, `"content":"OK"`) || !strings.Contains(b, `"object":"chat.completion"`) {
		t.Fatalf("caller got %s", b)
	}
	if gotPath != "/v1internal:streamGenerateContent?alt=sse" || !strings.HasPrefix(gotUA, "antigravity/cli/") {
		t.Errorf("upstream path=%s ua=%s", gotPath, gotUA)
	}
	for _, want := range []string{`"project":"proj-7"`, `"model":"gemini-3-flash"`, `"requestType":"agent"`, `"systemInstruction"`, `"sessionId":"-`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("envelope missing %s: %s", want, gotBody)
		}
	}
	// The project is kept for the next call.
	list, _ := s.ListConnections()
	if list[0].Meta["projectId"] != "proj-7" {
		t.Errorf("meta = %v", list[0].Meta)
	}
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].ConnectionID != c.ID || rows[0].InputTokens != 4 || rows[0].Model != "gemini-3-flash" {
		t.Errorf("usage = %+v", rows)
	}
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, loopbackRequest("GET", "/v1/models", nil))
	if b := mrec.Body.String(); !strings.Contains(b, `"antigravity/gemini-3-flash"`) || strings.Contains(b, "internal-x") {
		t.Errorf("models = %s", b)
	}
}

// Gemini takes a function name of at most 64 bytes, so a longer declared name goes out cut. A chat
// caller runs only the name it declared, so the call must come back under that name.
func TestGeminiChatCallerGetsLongToolNameBack(t *testing.T) {
	long := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__list_console_messages"
	var declared string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Write([]byte(`{"cloudaicompanionProject":"proj-7"}`))
		default:
			var env struct {
				Request struct {
					Tools []struct {
						FunctionDeclarations []struct {
							Name string `json:"name"`
						} `json:"functionDeclarations"`
					} `json:"tools"`
				} `json:"request"`
			}
			json.Unmarshal(b, &env)
			declared = ""
			if len(env.Request.Tools) > 0 && len(env.Request.Tools[0].FunctionDeclarations) > 0 {
				declared = env.Request.Tools[0].FunctionDeclarations[0].Name
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"responseId":"r","candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"`+declared+`","args":{}}}]},"finishReason":"STOP"}]}}`+"\n\n")
		}
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "ag", "fake-access-token")
	h := New(s, map[string]string{"antigravity": up.URL})

	for _, stream := range []bool{false, true} {
		body := `{"model":"antigravity/gemini-3-flash","stream":` + strconv.FormatBool(stream) + `,
			"messages":[{"role":"user","content":"go"}],
			"tools":[{"type":"function","function":{"name":"` + long + `","parameters":{"type":"object"}}}]}`
		rec := postV1(h, body)
		if len(declared) != 64 || !strings.HasPrefix(long, declared) {
			t.Fatalf("stream=%v: Gemini declaration %q, want the 64-byte cut", stream, declared)
		}
		b := rec.Body.String()
		if !strings.Contains(b, `"name":"`+long+`"`) {
			t.Errorf("stream=%v: caller got %s", stream, b)
		}
	}
}
