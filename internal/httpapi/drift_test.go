package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestDriftSeesANewClientFieldAndANewProviderField(t *testing.T) {
	answer := `{"id":"c","choices":[]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(answer))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	for i := 0; i < 4; i++ {
		postV1(h, `{"model":"groq/m","messages":[]}`)
	}
	answer = `{"id":"c","choices":[],"x_groq":{"id":"q"}}`
	postV1(h, `{"model":"groq/m","messages":[],"anti_cheat_nonce":"abc"}`)

	// The observer works in the background; poll the API until it has caught up.
	var body string
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/drift/changes?unacked=1", nil))
		body = rec.Body.String()
		if strings.Contains(body, "anti_cheat_nonce") && strings.Contains(body, "x_groq") {
			break
		}
		sleepMs(20)
	}
	if !strings.Contains(body, `"path":"anti_cheat_nonce"`) || !strings.Contains(body, `"direction":"request"`) {
		t.Errorf("client field not reported: %s", body)
	}
	if !strings.Contains(body, `"path":"x_groq"`) || !strings.Contains(body, `"direction":"response"`) {
		t.Errorf("provider field not reported: %s", body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/drift/ack", strings.NewReader(`{}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/drift/changes?unacked=1", nil))
	if !strings.Contains(rec.Body.String(), `"unacked":0`) {
		t.Errorf("after ack: %s", rec.Body.String())
	}
}

// sleepMs is a small pause while the background observer catches up.
func sleepMs(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }
