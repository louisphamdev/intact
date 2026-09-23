package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
			io.WriteString(w, `data: {"response":{"responseId":"r","modelVersion":"gemini-3-flash","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}}`+"\n\n")
		}
	}))
	defer up.Close()
	old := antigravityProdURL
	antigravityProdURL = up.URL
	defer func() { antigravityProdURL = old }()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("antigravity", "ag", "fake-access-token")
	h := New(s, map[string]string{"antigravity": up.URL})

	rec := postV1(h, `{"model":"antigravity/gemini-3-flash","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if b := rec.Body.String(); !strings.Contains(b, `"content":"OK"`) || !strings.Contains(b, `"object":"chat.completion"`) {
		t.Fatalf("caller got %s", b)
	}
	if gotPath != "/v1internal:streamGenerateContent?alt=sse" || !strings.HasPrefix(gotUA, "antigravity/ide/") {
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
	if len(rows) != 1 || rows[0].ConnectionID != c.ID || rows[0].InputTokens != 4 {
		t.Errorf("usage = %+v", rows)
	}
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, loopbackRequest("GET", "/v1/models", nil))
	if b := mrec.Body.String(); !strings.Contains(b, `"antigravity/gemini-3-flash"`) || strings.Contains(b, "internal-x") {
		t.Errorf("models = %s", b)
	}
}
