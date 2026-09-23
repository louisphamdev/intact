package httpapi

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestSetModelSplicesOnlyTheTopLevelValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"a","x":1}`, `{"model":"b","x":1}`},
		{`{ "stream" : true , "model" : "a" }`, `{ "stream" : true , "model" : "b" }`},
		{`{"messages":[{"model":"keep"}],"model":"a","n":1.50}`, `{"messages":[{"model":"keep"}],"model":"b","n":1.50}`},
		{`{"model":null}`, `{"model":"b"}`},
	}
	for _, c := range cases {
		got, ok := setModel([]byte(c.in), "b")
		if !ok || string(got) != c.want {
			t.Errorf("setModel(%s) = %s, %v; want %s", c.in, got, ok, c.want)
		}
	}
}

func TestSetModelLeavesOtherBodiesAlone(t *testing.T) {
	for _, in := range []string{``, `[1,2]`, `{"x":{"model":"a"}}`, `not json`} {
		got, ok := setModel([]byte(in), "b")
		if ok || string(got) != in {
			t.Errorf("setModel(%q) = %q, %v; want unchanged", in, got, ok)
		}
	}
}

func TestTopLevelCountSeesOnlyTheTopLevel(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`{"model":"a","x":1}`, 1},
		{`{"model":"a","x":1,"model":"b"}`, 2},
		{`{"messages":[{"model":"keep"}],"model":"a"}`, 1},
		{`{"x":1}`, 0},
		{`not json`, 0},
		{`[1,2]`, 0},
	}
	for _, c := range cases {
		if got := topLevelCount([]byte(c.in), "model"); got != c.want {
			t.Errorf("topLevelCount(%s) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A second top-level "model" key makes the model intact checked different from
// the model the provider reads, so the request never reaches an account.
func TestV1RefusesADuplicateTopLevelModelKey(t *testing.T) {
	f := &fakeProvider{models: `{"data":[{"id":"on-model"}]}`}
	url := f.start(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": url})

	for _, body := range []string{
		`{"model":"groq/on-model","messages":[{"role":"user","content":"hi"}],"model":"off-model"}`,
		`{"model":"on-model","messages":[{"role":"user","content":"hi"}],"model":"off-model"}`,
	} {
		if rec := postV1(h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d body = %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("upstream calls = %q, want none", calls)
	}

	// One "model" key still routes as before.
	rec := postV1(h, `{"model":"groq/on-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("single key: code = %d body = %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || !strings.Contains(f.calls[0], `"model":"on-model"`) {
		t.Errorf("upstream calls = %q, want one call on on-model", f.calls)
	}
}
