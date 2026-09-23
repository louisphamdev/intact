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
	answer := `{"id":"m","type":"message","content":[]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(answer))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "c", "k")
	h := New(s, map[string]string{"claude": up.URL})
	msg := func(b string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(b)))
	}
	for i := 0; i < 4; i++ {
		msg(`{"model":"claude/m","max_tokens":1,"messages":[]}`)
	}
	answer = `{"id":"m","type":"message","content":[],"x_new":{"id":"q"}}`
	msg(`{"model":"claude/m","max_tokens":1,"messages":[],"anti_cheat_nonce":"abc"}`)

	// The observer works in the background; poll the API until it has caught up.
	var body string
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("GET", "/api/drift/changes?unacked=1", nil))
		body = rec.Body.String()
		if strings.Contains(body, "anti_cheat_nonce") && strings.Contains(body, "x_new") {
			break
		}
		sleepMs(20)
	}
	if !strings.Contains(body, `"path":"anti_cheat_nonce"`) || !strings.Contains(body, `"direction":"request"`) {
		t.Errorf("client field not reported: %s", body)
	}
	if !strings.Contains(body, `"path":"x_new"`) || !strings.Contains(body, `"direction":"response"`) {
		t.Errorf("provider field not reported: %s", body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/api/drift/ack", strings.NewReader(`{}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/api/drift/changes?unacked=1", nil))
	if !strings.Contains(rec.Body.String(), `"unacked":0`) {
		t.Errorf("after ack: %s", rec.Body.String())
	}
}

// sleepMs is a small pause while the background observer catches up.
func sleepMs(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }

func TestDriftIgnoresADocumentedAPI(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"choices":[]}`)) }))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	for i := 0; i < 6; i++ {
		postV1(h, `{"model":"groq/m","messages":[],"field_`+string(rune('a'+i))+`":1}`)
	}
	sleepMs(100)
	if c, _ := s.ListShapeChanges(store.ShapeChangeFilter{}); len(c) != 0 {
		t.Errorf("groq traffic was watched: %+v", c)
	}
}

func TestDriftFieldsEmptyIsAList(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/drift/fields?provider=codex", nil))
	if !strings.Contains(rec.Body.String(), `"fields":[]`) {
		t.Errorf("fields = %s", rec.Body.String())
	}
}
