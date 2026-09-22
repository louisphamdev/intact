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

func TestModelTableSwitchDropAndTest(t *testing.T) {
	list := `{"data":[{"id":"a"},{"id":"b"}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(list))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	do := func(method, path, body string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Body.String()
	}
	if b := do("GET", "/providers/groq/model-table", ""); !strings.Contains(b, `"model":"a"`) || !strings.Contains(b, `"active":true`) {
		t.Fatalf("table = %s", b)
	}
	// Switch b off: gone from /v1/models, refused by prefix.
	do("POST", "/providers/groq/models/active", `{"models":["b"],"active":false}`)
	if b := do("GET", "/v1/models", ""); strings.Contains(b, `"groq/b"`) || !strings.Contains(b, `"groq/a"`) {
		t.Errorf("v1/models = %s", b)
	}
	rec := postV1(h, `{"model":"groq/b","messages":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("switched-off model: code=%d", rec.Code)
	}
	// A test still reaches it, and records the result.
	var res modelTest
	json.Unmarshal([]byte(do("POST", "/providers/groq/models/test", `{"model":"b"}`)), &res)
	if !res.OK || res.Message != "OK" {
		t.Errorf("test = %+v", res)
	}
	// The provider drops a: a successful fetch deletes it.
	list = `{"data":[{"id":"b"}]}`
	b := do("GET", "/providers/groq/model-table?refresh=1", "")
	var tbl struct{ Models []store.ProviderModel }
	json.Unmarshal([]byte(b), &tbl)
	seen := map[string]bool{}
	for _, m := range tbl.Models {
		seen[m.Model] = true
		if m.Model == "b" && (!m.TestOK || m.Active) {
			t.Errorf("b = %+v", m)
		}
	}
	if seen["a"] || !seen["b"] {
		t.Errorf("after the fetch: %v", seen)
	}
}
