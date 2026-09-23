package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

func postJSONTo(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", path, strings.NewReader(body)))
	return rec
}

func TestDeclaredAPIKeyProvider(t *testing.T) {
	var gotAuth, gotHeader, gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v9/list":
			w.Write([]byte(`[{"id":"models/alpha"},{"id":"beta"}]`))
		default:
			b, _ := io.ReadAll(r.Body)
			gotAuth, gotHeader, gotPath, gotBody = r.Header.Get("X-Key"), r.Header.Get("X-Team"), r.URL.Path, string(b)
			w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		}
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	def := `{"id":"acme","name":"Acme AI","kind":"apikey","api":"openai","baseUrl":"` + up.URL + `/v1","authHeader":"X-Key","authPrefix":"",
		"headers":{"X-Team":"t1"},"modelsUrl":"` + up.URL + `/v9/list"}`
	if rec := postJSONTo(h, "/provider-defs", def); rec.Code != 200 {
		t.Fatalf("declare: %d %s", rec.Code, rec.Body.String())
	}
	// An account: a pasted key, as for a built-in provider.
	rec := httptest.NewRecorder()
	areq := loopbackRequest("POST", "/accounts", strings.NewReader(url.Values{"provider": {"acme"}, "label": {"k"}, "secret": {"sk-1"}}.Encode()))
	areq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, areq)
	if rec.Code >= 400 {
		t.Fatalf("account: %d %s", rec.Code, rec.Body.String())
	}
	// The list: a bare array, "models/" dropped.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/v1/models", nil))
	if b := rec.Body.String(); !strings.Contains(b, `"acme/alpha"`) || !strings.Contains(b, `"acme/beta"`) {
		t.Errorf("models = %s", b)
	}
	rec = postV1(h, `{"model":"acme/alpha","messages":[]}`)
	if rec.Code != 200 || gotAuth != "sk-1" || gotHeader != "t1" || gotPath != "/v1/chat/completions" || !strings.Contains(gotBody, `"model":"alpha"`) {
		t.Errorf("call: %d auth=%q team=%q path=%s body=%s", rec.Code, gotAuth, gotHeader, gotPath, gotBody)
	}
	// It is listed with its name, and refuses deletion while it has an account.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/providers", nil))
	if !strings.Contains(rec.Body.String(), `"name":"Acme AI"`) {
		t.Errorf("providers = %s", rec.Body.String())
	}
	if rec := postJSONTo(h, "/provider-defs/acme/delete", ""); rec.Code != 400 {
		t.Errorf("delete with an account: %d", rec.Code)
	}
}

func TestDeclaredFixedModelsAndAnthropic(t *testing.T) {
	var gotKey, gotVer, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVer, gotPath = r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version"), r.URL.Path
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	if rec := postJSONTo(h, "/provider-defs", `{"id":"claudish","api":"anthropic","baseUrl":"`+up.URL+`/v1","modelsUrl":"none","models":["m-1","m-2"]}`); rec.Code != 200 {
		t.Fatalf("declare: %s", rec.Body.String())
	}
	c, _ := s.CreateConnection("claudish", "k", "ak")
	_ = c
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/providers/claudish/model-table", nil))
	if b := rec.Body.String(); !strings.Contains(b, `"model":"m-1"`) || !strings.Contains(b, `"model":"m-2"`) {
		t.Errorf("table = %s", b)
	}
	rec = postV1(h, `{"model":"claudish/m-1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || gotKey != "ak" || gotVer != "2023-06-01" || gotPath != "/v1/messages" {
		t.Errorf("call: %d key=%q ver=%q path=%s %s", rec.Code, gotKey, gotVer, gotPath, rec.Body.String())
	}
}

func TestDeclareValidation(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	for body, want := range map[string]string{
		`{"id":"groq","baseUrl":"https://x.dev/v1"}`:               "built-in",
		`{"id":"Bad Id","baseUrl":"https://x.dev/v1"}`:             "id:",
		`{"id":"x","baseUrl":"ftp://x"}`:                           "baseUrl",
		`{"id":"x","baseUrl":"https://x.dev","api":"grpc"}`:        "api:",
		`{"id":"x","baseUrl":"https://x.dev","modelsUrl":"none"}`:  "models:",
		`{"id":"x","baseUrl":"https://x.dev","kind":"oauth-code"}`: "oauth:",
		`{"id":"x","baseUrl":"https://x.dev","kind":"oauth-code","oauth":{"clientId":"c","tokenUrl":"https://t.dev","authorizeUrl":"https://a.dev"}}`: "redirectUri",
		`{"id":"x","baseUrl":"https://x.dev","kind":"oauth-device","oauth":{"clientId":"c","tokenUrl":"https://t.dev"}}`:                              "deviceCodeUrl",
	} {
		rec := postJSONTo(h, "/provider-defs", body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s -> %d %s (want %s)", body, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestDeclaredOAuthFlows(t *testing.T) {
	polls := 0
	var tokenForm url.Values
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/device":
			w.Write([]byte(`{"device_code":"dc1","user_code":"ABCD-1234","verification_uri":"https://auth.example/activate","interval":1}`))
		case "/token":
			tokenForm = r.PostForm
			if r.PostForm.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
				polls++
				if polls == 1 {
					w.Write([]byte(`{"error":"authorization_pending"}`))
					return
				}
			}
			w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		}
	}))
	defer auth.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	postJSONTo(h, "/provider-defs", `{"id":"devco","kind":"oauth-device","baseUrl":"https://api.devco.dev/v1","oauth":{"deviceCodeUrl":"`+auth.URL+`/device","tokenUrl":"`+auth.URL+`/token","clientId":"cid","scope":"chat"}}`)
	rec := postJSONTo(h, "/oauth/devco/start", `{"label":"Work"}`)
	var start struct {
		DeviceCode, UserCode, VerificationURI string
	}
	json.Unmarshal(rec.Body.Bytes(), &start)
	if start.UserCode != "ABCD-1234" || start.VerificationURI != "https://auth.example/activate" {
		t.Fatalf("device start = %s", rec.Body.String())
	}
	if b := postJSONTo(h, "/oauth/devco/poll", `{"deviceCode":"dc1","label":"Work"}`).Body.String(); !strings.Contains(b, "pending") {
		t.Errorf("first poll = %s", b)
	}
	if b := postJSONTo(h, "/oauth/devco/poll", `{"deviceCode":"dc1","label":"Work"}`).Body.String(); !strings.Contains(b, `"status":"done"`) {
		t.Errorf("second poll = %s", b)
	}
	conns, _ := s.ListConnections()
	if len(conns) != 1 || conns[0].Label != "Work" {
		t.Fatalf("conns = %+v", conns)
	}
	creds, _ := s.OAuth(conns[0].ID)
	if creds.RefreshToken != "rt" || creds.TokenURL != auth.URL+"/token" || creds.ClientID != "cid" {
		t.Errorf("creds = %+v", creds)
	}
	// Browser flow: the authorize URL carries the declared settings and PKCE.
	postJSONTo(h, "/provider-defs", `{"id":"codeco","kind":"oauth-code","baseUrl":"https://api.codeco.dev/v1","oauth":{"authorizeUrl":"https://auth.codeco.dev/authorize","tokenUrl":"`+auth.URL+`/token","clientId":"cc","clientSecret":"shh","scope":"a b","redirectUri":"http://localhost:9999/cb","extra":{"access_type":"offline"}}}`)
	rec = postJSONTo(h, "/oauth/codeco/start", `{}`)
	var st struct{ URL, State string }
	json.Unmarshal(rec.Body.Bytes(), &st)
	u, _ := url.Parse(st.URL)
	q := u.Query()
	if u.Host != "auth.codeco.dev" || q.Get("client_id") != "cc" || q.Get("redirect_uri") != "http://localhost:9999/cb" || q.Get("code_challenge") == "" || q.Get("access_type") != "offline" {
		t.Fatalf("authorize url = %s", st.URL)
	}
	rec = postJSONTo(h, "/oauth/codeco/finish", `{"state":"`+st.State+`","input":"http://localhost:9999/cb?code=xyz&state=`+st.State+`"}`)
	if rec.Code != 200 || tokenForm.Get("code") != "xyz" || tokenForm.Get("client_secret") != "shh" || tokenForm.Get("code_verifier") == "" {
		t.Errorf("finish: %d %s form=%v", rec.Code, rec.Body.String(), tokenForm)
	}
}

func TestLegacyCustomEndpointBecomesDeclared(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("oldco", "k", "x")
	s.SetBaseURL(c.ID, "https://old.example/v1")
	s.SetMeta(c.ID, map[string]string{"api": "anthropic"})
	New(s, nil)
	defs, _ := s.ProviderDefs()
	if len(defs) != 1 || defs[0].ID != "oldco" || defs[0].API != "anthropic" || defs[0].BaseURL != "https://old.example/v1" {
		t.Errorf("defs = %+v", defs)
	}
	if p, ok := provider.Lookup("oldco"); !ok || p.API != "anthropic" {
		t.Errorf("lookup = %+v %v", p, ok)
	}
}
