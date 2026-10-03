package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A profile can name the folded base (antigravity/gemini-3.8-flash) for the CLI.
// Code Assist knows only the level variants and answers 404 for the base, so the
// Bifrost body must carry the variant that intact picked, like the convert path.
func TestBifrostAntigravityBaseNameSendsAVariant(t *testing.T) {
	up, got := agyBifrostServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("antigravity", "pool", "token-a")
	h := New(s, map[string]string{"antigravity": up.URL})

	body := strings.Replace(agyBody, `"antigravity/gemini-3.8-flash-high"`, `"antigravity/gemini-3.8-flash"`, 1)
	req := loopbackRequest("POST", "/v1/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
	req.Header.Set("User-Agent", agyCLIUA)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sent struct {
		Model   string `json:"model"`
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if base, level := splitVariant(sent.Model); base != "gemini-3.8-flash" || level == "" {
		t.Errorf("upstream model = %q, want a level variant of gemini-3.8-flash", sent.Model)
	}
	if sent.Project != "pool-project" {
		t.Errorf("project = %q", sent.Project)
	}
}
