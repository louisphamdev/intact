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

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// sendJSON runs one management request with a JSON body, without a token (an
// open server, as the tests build).
func sendJSON(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := loopbackRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// jsonUnmarshal decodes a recorder's body into v, failing the test on a body
// that is not the JSON v expects.
func jsonUnmarshal(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
}

func TestProfileAPI(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	other, _ := s.CreateConnection("openai", "three", "k3")
	a, h := newServer(s, nil, nil)

	for _, bad := range []struct{ body, why string }{
		{`{"provider":"","name":"lite"}`, "no provider"},
		{`{"provider":"nope","name":"lite"}`, "unknown provider"},
		{`{"provider":"groq","name":""}`, "no name"},
		{`{"provider":"groq","name":"Lite"}`, "uppercase name"},
		{`{"provider":"groq","name":"lite","members":["` + other.ID + `"]}`, "member of another provider"},
		{`{"provider":"groq","name":"lite","members":["zzz"]}`, "unknown member"},
	} {
		if rec := sendJSON(h, "POST", "/api/profiles", bad.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s, want 400", bad.why, rec.Code, rec.Body.String())
		}
	}

	// A profile whose prefix a provider already owns is refused: that
	// provider would win the routing, so the profile could never be reached.
	d := provider.Def{ID: "groq-lite", BaseURL: "http://127.0.0.1:9/v1"}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize def: %v", err)
	}
	if err := s.PutProviderDef(d); err != nil {
		t.Fatalf("put def: %v", err)
	}
	a.loadDefs()
	if rec := sendJSON(h, "POST", "/api/profiles", `{"provider":"groq","name":"lite"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("profile prefix taken by a provider: code=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

// A model list of its own is checked against the provider's: a profile cannot
// be configured with a model no account of that provider serves.
func TestProfileAPIRefusesAnUnknownModel(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	groq1, _ := s.CreateConnection("groq", "one", "k1")
	_, h := newServer(s, nil, nil)
	s.SyncModels("groq", []string{"llama"}, true, nil)

	if rec := sendJSON(h, "POST", "/api/profiles", `{"provider":"groq","name":"lite",
		"members":["`+groq1.ID+`"],"models":["nope"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown model: %d %s, want 400", rec.Code, rec.Body.String())
	}
	rec := sendJSON(h, "POST", "/api/profiles", `{"provider":"groq","name":"lite",
		"members":["`+groq1.ID+`"],"models":["llama"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("known model: %d %s", rec.Code, rec.Body.String())
	}
	var created store.Profile
	jsonUnmarshal(t, rec, &created)
	if len(created.Models) != 1 || created.Models[0] != "llama" {
		t.Fatalf("created models: %+v", created.Models)
	}
}

// The rest of the CRUD runs without the colliding def, in its own store.
func TestProfileAPICRUD(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	groq1, _ := s.CreateConnection("groq", "one", "k1")
	groq2, _ := s.CreateConnection("groq", "two", "k2")
	_, h := newServer(s, nil, nil)

	rec := sendJSON(h, "POST", "/api/profiles",
		`{"provider":"groq","name":"lite","members":["`+groq1.ID+`","`+groq2.ID+`"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created store.Profile
	jsonUnmarshal(t, rec, &created)
	if len(created.Members) != 2 || created.Members[0] != groq1.ID {
		t.Fatalf("created: %+v", created)
	}

	rec = sendJSON(h, "GET", "/api/profiles", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Profiles []store.Profile `json:"profiles"`
	}
	jsonUnmarshal(t, rec, &list)
	if len(list.Profiles) != 1 || list.Profiles[0].ID != created.ID {
		t.Fatalf("listed: %+v", list)
	}

	// The same name on the same provider is refused.
	if rec := sendJSON(h, "POST", "/api/profiles",
		`{"provider":"groq","name":"lite"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d %s, want 409", rec.Code, rec.Body.String())
	}

	// A move of members keeps the name; an unknown id is 404.
	rec = sendJSON(h, "PUT", "/api/profiles/"+created.ID, `{"members":["`+groq2.ID+`"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var updated store.Profile
	jsonUnmarshal(t, rec, &updated)
	if updated.Name != "lite" || len(updated.Members) != 1 || updated.Members[0] != groq2.ID {
		t.Fatalf("updated: %+v", updated)
	}
	if rec := sendJSON(h, "PUT", "/api/profiles/zzz", `{"members":[]}`); rec.Code != http.StatusNotFound {
		t.Errorf("update of no profile: %d, want 404", rec.Code)
	}

	if rec := sendJSON(h, "DELETE", "/api/profiles/"+created.ID, ``); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := sendJSON(h, "DELETE", "/api/profiles/"+created.ID, ``); rec.Code != http.StatusNotFound {
		t.Errorf("delete twice: %d, want 404", rec.Code)
	}
}

// profileUpstream answers a provider's calls, and records how often each
// account's secret reached its /chat/completions.
func profileUpstream(t *testing.T, fail map[string]bool) (*httptest.Server, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			calls["models:"+secret]++
			mu.Unlock()
			w.Write([]byte(`{"data":[{"id":"llama"}]}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls[secret]++
		mu.Unlock()
		if fail[secret] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"llama","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)
	return up, func() map[string]int { mu.Lock(); defer mu.Unlock(); return calls }
}

func TestProfileRoutingServesItsMembersAlone(t *testing.T) {
	up, calls := profileUpstream(t, map[string]bool{"s1": true, "s2": true})
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c1, _ := s.CreateConnection("groq", "one", "s1")
	c2, _ := s.CreateConnection("groq", "two", "s2")
	c3, _ := s.CreateConnection("groq", "three", "s3")
	s.SetStandby(c3.ID, true)
	_, h := newServer(s, map[string]string{"groq": up.URL}, nil)

	if rec := sendJSON(h, "POST", "/api/profiles",
		`{"provider":"groq","name":"lite","members":["`+c1.ID+`","`+c2.ID+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}

	// A profile call is served by its members alone. Both members fail, and
	// the provider's standby stays untouched: the last member's answer is
	// what the caller sees.
	rec := postV1(h, `{"model":"groq-lite/llama","messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("profile call: %d %s, want the last member's 500", rec.Code, rec.Body.String())
	}
	if n := calls()["s3"]; n != 0 {
		t.Fatalf("the standby served %d profile calls, want 0", n)
	}

	// The provider's own prefix keeps every account, standby last: after both
	// members fail, the standby answers.
	rec = postV1(h, `{"model":"groq/llama","messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("provider call: %d %s, want 200 from the standby", rec.Code, rec.Body.String())
	}
	if n := calls()["s3"]; n != 1 {
		t.Fatalf("the standby answered %d provider calls, want 1", n)
	}
}

func TestProfileModelLookup(t *testing.T) {
	up, _ := profileUpstream(t, nil)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c1, _ := s.CreateConnection("groq", "one", "k1")
	_, h := newServer(s, map[string]string{"groq": up.URL}, nil)
	if rec := sendJSON(h, "POST", "/api/profiles",
		`{"provider":"groq","name":"lite","members":["`+c1.ID+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/groq-lite/llama", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"groq-lite/llama"`) {
		t.Fatalf("profile model lookup: %d %s", rec.Code, rec.Body.String())
	}
}

// A profile with a model list of its own serves only that list: a call for
// another model of the same provider is refused with 404, while the
// provider's own prefix keeps serving it.
func TestProfileModelListServesOnlyItsModels(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c1, _ := s.CreateConnection("groq", "one", "s1")
	_, h := newServer(s, map[string]string{"groq": "http://127.0.0.1:1/v1"}, nil)
	s.SyncModels("groq", []string{"llama", "mixtral"}, true, nil)
	rec := sendJSON(h, "POST", "/api/profiles", `{"provider":"groq","name":"lite",
		"members":["`+c1.ID+`"],"models":["llama"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	// The listed model is looked up under the profile's prefix.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/groq-lite/llama", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"groq-lite/llama"`) {
		t.Fatalf("listed model: %d %s", rec.Code, rec.Body.String())
	}
	// The model the list does not name is not served by the profile.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models/groq-lite/mixtral", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unlisted model lookup: %d, want 404", rec.Code)
	}
	if got := postV1(h, `{"model":"groq-lite/mixtral","messages":[]}`); got.Code != http.StatusNotFound {
		t.Errorf("unlisted model call: %d %s, want 404", got.Code, got.Body.String())
	}
	// The provider's own prefix is not limited by the profile's list: its
	// call reaches the (unreachable) upstream, so it fails there rather than
	// at intact's own routing.
	if got := postV1(h, `{"model":"groq/mixtral","messages":[]}`); got.Code == http.StatusNotFound {
		t.Errorf("provider prefix: %d %s, want anything but intact's 404", got.Code, got.Body.String())
	}
}

// A profile fetches its models through its own accounts, and its rotation
// cursor moves apart from the provider's: a call through the profile moves
// the profile's turn and leaves the provider's where it was.
func TestProfileModelsAndRotation(t *testing.T) {
	up, calls := profileUpstream(t, nil)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c1, _ := s.CreateConnection("groq", "one", "s1")
	c2, _ := s.CreateConnection("groq", "two", "s2")
	_, h := newServer(s, map[string]string{"groq": up.URL}, nil)
	s.SyncModels("groq", []string{"llama", "mixtral"}, true, nil)
	rec := sendJSON(h, "POST", "/api/profiles", `{"provider":"groq","name":"lite",
		"members":["`+c1.ID+`","`+c2.ID+`"],"models":["llama"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var pr store.Profile
	jsonUnmarshal(t, rec, &pr)

	// The profile's own list, and a fetch through its own account.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/api/profiles/"+pr.ID+"/models", nil))
	var got struct {
		Models []string `json:"models"`
	}
	jsonUnmarshal(t, rec, &got)
	if len(got.Models) != 1 || got.Models[0] != "llama" {
		t.Fatalf("profile models: %+v", got.Models)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/api/profiles/"+pr.ID+"/models?refresh=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	if c := calls(); c["models:s1"] == 0 && c["models:s2"] == 0 {
		t.Errorf("the refresh reached no account of the profile: %+v", c)
	}

	// Whose turn it is inside the profile, before and after a call through it.
	rotationOf := func() (string, int) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("GET", "/api/profiles/"+pr.ID+"/rotation", nil))
		var st struct {
			Next string `json:"next"`
			Used int    `json:"used"`
		}
		jsonUnmarshal(t, rec, &st)
		return st.Next, st.Used
	}
	first, _ := rotationOf()
	if first != c1.ID {
		t.Fatalf("profile's first turn: %s, want %s", first, c1.ID)
	}
	if rec := postV1(h, `{"model":"groq-lite/llama","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("call through the profile: %d %s", rec.Code, rec.Body.String())
	}
	next, used := rotationOf()
	if next != c2.ID || used != 0 {
		t.Fatalf("after one call: next=%s used=%d, want %s and 0", next, used, c2.ID)
	}
	// The provider's own cursor is untouched by the profile's call.
	provNext := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("GET", "/api/providers/groq/rotation", nil))
		var st struct {
			Next string `json:"next"`
		}
		jsonUnmarshal(t, rec, &st)
		return st.Next
	}
	if n := provNext(); n != c1.ID {
		t.Fatalf("provider's turn after a profile call: %s, want %s (the profile moved only its own)", n, c1.ID)
	}
}
