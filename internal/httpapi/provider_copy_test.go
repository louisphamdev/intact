package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

func postWith(t *testing.T, h http.Handler, ck *http.Cookie, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := loopbackRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// An account moves between a built-in provider and its copy, and nowhere else: the copy
// calls the same upstream, so the credential works there unchanged.
func TestMoveAccountToACopy(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	defer provider.SetDeclared(nil)
	keep, _ := s.CreateConnection("antigravity", "keep", "k1")
	move, _ := s.CreateConnection("antigravity", "move", "k2")
	other, _ := s.CreateConnection("groq", "g", "k3")
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)

	if rec := postWith(t, h, ck, "/provider-defs", `{"id":"antigravity-lite","name":"Antigravity Lite","of":"antigravity"}`); rec.Code != http.StatusOK {
		t.Fatalf("declare copy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postWith(t, h, ck, "/accounts/"+move.ID+"/provider", `{"provider":"antigravity-lite"}`); rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ body, id string }{
		{`{"provider":"groq"}`, keep.ID},                   // another upstream
		{`{"provider":"antigravity-lite"}`, other.ID},      // groq is not antigravity
		{`{"provider":"nope"}`, keep.ID},                   // unknown
		{`{"provider":"antigravity-lite"}`, "no-such-one"}, // unknown account
	} {
		if rec := postWith(t, h, ck, "/accounts/"+c.id+"/provider", c.body); rec.Code == http.StatusOK {
			t.Errorf("move %s of %s was accepted", c.body, c.id)
		}
	}
	got := map[string]string{}
	list, _ := s.ListConnections()
	for _, c := range list {
		got[c.Label] = c.Provider
	}
	if got["keep"] != "antigravity" || got["move"] != "antigravity-lite" || got["g"] != "groq" {
		t.Errorf("providers after the moves = %v", got)
	}
}

// A copy has its own accounts: a call to the copy uses only them, and a call to the
// original never reaches them. The copy also runs the original's upstream and envelope.
func TestCopyAndOriginalUseTheirOwnAccounts(t *testing.T) {
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			w.Write([]byte(`{"cloudaicompanionProject":"proj-7"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			w.Write([]byte(`{"models":{"gemini-3-flash":{}}}`))
		default:
			seen[r.Header.Get("Authorization")]++
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"responseId":"r","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`+"\n\n")
		}
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	defer provider.SetDeclared(nil)
	if err := s.PutProviderDef(provider.Def{ID: "antigravity-lite", Name: "Antigravity Lite", Of: "antigravity"}); err != nil {
		t.Fatal(err)
	}
	s.CreateConnection("antigravity", "main", "tok-main")
	s.CreateConnection("antigravity-lite", "lite1", "tok-lite1")
	s.CreateConnection("antigravity-lite", "lite2", "tok-lite2")
	h := New(s, map[string]string{"antigravity": up.URL})

	for i := 0; i < 4; i++ {
		// One conversation per call: the same conversation stays on one account.
		if rec := postV1(h, `{"model":"antigravity-lite/gemini-3-flash","messages":[{"role":"user","content":"lite `+strconv.Itoa(i)+`"}]}`); !strings.Contains(rec.Body.String(), "OK") {
			t.Fatalf("copy call: %d %s", rec.Code, rec.Body.String())
		}
	}
	for i := 0; i < 3; i++ {
		if rec := postV1(h, `{"model":"antigravity/gemini-3-flash","messages":[{"role":"user","content":"main `+strconv.Itoa(i)+`"}]}`); !strings.Contains(rec.Body.String(), "OK") {
			t.Fatalf("original call: %d %s", rec.Code, rec.Body.String())
		}
	}
	if seen["Bearer tok-main"] != 3 || seen["Bearer tok-lite1"] != 2 || seen["Bearer tok-lite2"] != 2 {
		t.Errorf("calls per account = %v, want main 3, lite1 2, lite2 2", seen)
	}
}

// A copy runs its original's request rules, then its own; the original never runs the copy's.
func TestCopyRunsTheOriginalsFilters(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	defer provider.SetDeclared(nil)
	s.PutProviderDef(provider.Def{ID: "antigravity-lite", Name: "Antigravity Lite", Of: "antigravity"})
	a, _ := newServer(s, nil, nil)
	for _, f := range []store.Filter{
		{Provider: "antigravity", Kind: "field", Pattern: "from_original", Enabled: true},
		{Provider: "antigravity-lite", Kind: "field", Pattern: "from_copy", Enabled: true},
	} {
		if _, err := s.SaveFilter(f); err != nil {
			t.Fatal(err)
		}
	}
	a.reloadFilters()
	has := func(prov, pattern string) bool {
		for _, r := range a.rulesFor(prov) {
			if r.Pattern == pattern {
				return true
			}
		}
		return false
	}
	if !has("antigravity-lite", "from_original") || !has("antigravity-lite", "from_copy") {
		t.Error("the copy misses its original's rules or its own")
	}
	if has("antigravity", "from_copy") {
		t.Error("the original runs the copy's rule")
	}
}

// A new copy starts with its original's model switches and model policy, so it serves the
// same models until the operator changes them; later changes stay apart.
func TestNewCopyStartsWithTheOriginalsModels(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	defer provider.SetDeclared(nil)
	s.SyncModels("antigravity", []string{"m-on", "m-off"}, true, func(string) bool { return true })
	s.SetModelsActive("antigravity", []string{"m-off"}, false)
	s.SetSetting(policyKey("antigravity"), `{"autoTest":false,"onlyFree":true}`)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	ck := loginCookie(t, h, cfg)
	if rec := postWith(t, h, ck, "/provider-defs", `{"id":"antigravity-lite","name":"Antigravity Lite","of":"antigravity"}`); rec.Code != http.StatusOK {
		t.Fatalf("declare copy: %d %s", rec.Code, rec.Body.String())
	}
	off := s.InactiveModels("antigravity-lite")
	list, _ := s.ListModels("antigravity-lite")
	if len(list) != 2 || !off["m-off"] || off["m-on"] {
		t.Errorf("copy models = %+v, off = %v", list, off)
	}
	if v, _ := s.GetSetting(policyKey("antigravity-lite")); v != `{"autoTest":false,"onlyFree":true}` {
		t.Errorf("copy policy = %q", v)
	}
	// Saving the copy again does not reset what the operator changed since.
	s.SetModelsActive("antigravity-lite", []string{"m-off"}, true)
	postWith(t, h, ck, "/provider-defs", `{"id":"antigravity-lite","name":"Antigravity Lite 2","of":"antigravity"}`)
	if s.InactiveModels("antigravity-lite")["m-off"] {
		t.Error("saving the copy again reset its model switches")
	}
}
