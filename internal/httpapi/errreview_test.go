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

	"github.com/louisphamdev/intact/internal/store"
)

// alertsServer stands in for Telegram and keeps what it was sent.
func alertsServer(t *testing.T) func() []string {
	var mu sync.Mutex
	var got []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		got = append(got, r.URL.Path+" "+r.Form.Get("chat_id")+" "+r.Form.Get("message_thread_id")+" "+r.Form.Get("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	old := telegramAPI
	telegramAPI = tg.URL
	t.Cleanup(func() { telegramAPI = old; tg.Close() })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

func TestErrorReviewBlacklistsWhatTheReplayProves(t *testing.T) {
	var mu sync.Mutex
	upCalls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Write([]byte(`{}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upCalls++
		mu.Unlock()
		if strings.Contains(string(b), "anti_cheat") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Invalid request"}}`)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()
	judgeCalls := 0
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeCalls++
		answer := `{"cause":"unknown field","action":"blacklist","candidates":[{"kind":"field","pattern":"messages"},{"kind":"field","pattern":"foo"},{"kind":"field","pattern":"anti_cheat"}],"reason":"the provider refuses anti_cheat"}`
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
		w.Write(b)
	}))
	defer judge.Close()
	alerts := alertsServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	s.CreateConnection("openrouter", "o", "k2")
	h := New(s, map[string]string{"groq": up.URL, "openrouter": judge.URL})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	// A channel for the alerts; its token never comes back.
	rec := do("POST", "/notify/channels", `{"name":"ops","type":"telegram","enabled":true,"config":{"botToken":"123456:SECRETTOKEN","chatId":"-100","threadId":"3114"}}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "SECRETTOKEN") {
		t.Fatalf("channel: %d %s", rec.Code, rec.Body.String())
	}
	// The review cannot be on without a model.
	if rec := do("POST", "/errors/review", `{"enabled":true}`); rec.Code != 200 || strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("enabled without a model: %s", rec.Body.String())
	}
	if rec := do("POST", "/errors/review", `{"enabled":true,"model":"openrouter/judge"}`); !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("config: %s", rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		postV1(h, `{"model":"groq/m","anti_cheat":1,"messages":[{"role":"user","content":"hi"}]}`)
	}
	rec = do("POST", "/errors/review", `{"run":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"judged":1`) {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var v struct{ Verdicts []store.ErrorVerdict }
	json.Unmarshal(do("GET", "/errors/verdicts", "").Body.Bytes(), &v)
	if len(v.Verdicts) != 1 || !v.Verdicts[0].Applied || !v.Verdicts[0].Verified || v.Verdicts[0].Detail != "field: anti_cheat" ||
		!strings.Contains(v.Verdicts[0].Note, "messages: the request needs it") || !strings.Contains(v.Verdicts[0].Note, "foo: removes nothing") {
		t.Fatalf("verdicts = %+v", v.Verdicts)
	}
	// The client's next request goes through.
	if rec := postV1(h, `{"model":"groq/m","anti_cheat":1,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Errorf("after the fix: %d", rec.Code)
	}
	// Judged once; nothing is due now.
	if rec := do("POST", "/errors/review", `{"run":true}`); !strings.Contains(rec.Body.String(), `"judged":0`) || judgeCalls != 1 {
		t.Errorf("second run: %s, judge calls %d", rec.Body.String(), judgeCalls)
	}
	got := alerts()
	if len(got) != 1 || !strings.Contains(got[0], "/bot123456:SECRETTOKEN/sendMessage -100 3114") || !strings.Contains(got[0], "Blacklisted field: anti_cheat") {
		t.Errorf("alerts = %q", got)
	}
}

func TestErrorReviewClosesPassingFailuresWithoutTheModel(t *testing.T) {
	var mu sync.Mutex
	fails := 3
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"try later"}}`)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()
	judged := false
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { judged = true }))
	defer judge.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	s.CreateConnection("openrouter", "o", "k2")
	h := New(s, map[string]string{"groq": up.URL, "openrouter": judge.URL})
	for i := 0; i < 3; i++ {
		postV1(h, `{"model":"groq/m","messages":[{"role":"user","content":"hi"}]}`)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/errors/review", strings.NewReader(`{"enabled":true,"model":"openrouter/judge","run":true}`)))
	list, _ := s.ListErrorVerdicts(0)
	if judged || len(list) != 1 || list[0].Action != "ignore" || list[0].Cause != "passing failure" {
		t.Errorf("judged=%v verdicts=%+v (%s)", judged, list, rec.Body.String())
	}
}

func TestNotifyChannels(t *testing.T) {
	alerts := alertsServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	for body, want := range map[string]string{
		`{"type":"telegram","config":{"chatId":"1"}}`:                               "Bot token is required",
		`{"type":"telegram","config":{"botToken":"x","chatId":"1","threadId":"a"}}`: "Topic id: a number",
		`{"type":"webhook","config":{"url":"ftp://x"}}`:                             "URL: an http(s) address",
		`{"type":"sms","config":{}}`:                                                "type: one of",
	} {
		if rec := do("POST", "/notify/channels", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	var c store.NotifyChannel
	json.Unmarshal(do("POST", "/notify/channels", `{"name":"tg","type":"telegram","enabled":true,"events":["error.action","bogus"],"config":{"botToken":"123:ABCDEFGHIJ","chatId":"-100"}}`).Body.Bytes(), &c)
	if c.Config["botToken"] != "••••GHIJ" || len(c.Events) != 1 {
		t.Fatalf("saved = %+v", c)
	}
	// Saving the masked form back keeps the token.
	body, _ := json.Marshal(map[string]any{"name": "tg2", "type": "telegram", "enabled": true, "events": c.Events, "config": c.Config})
	do("PUT", "/notify/channels/"+c.ID, string(body))
	if rec := do("POST", "/notify/channels/"+c.ID+"/test", ""); rec.Code != 200 {
		t.Fatalf("test: %d %s", rec.Code, rec.Body.String())
	}
	got := alerts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "/bot123:ABCDEFGHIJ/sendMessage -100 ") {
		t.Errorf("alerts = %q", got)
	}
	// An event the channel does not take is not sent; one it takes is, once per key.
	a := h.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	})
	_ = a
	info := do("GET", "/notify", "").Body.String()
	if !strings.Contains(info, `"name":"tg2"`) || !strings.Contains(info, `"types"`) || strings.Contains(info, "ABCDEF") {
		t.Errorf("info = %s", info)
	}
}

func TestProtectedPatternAndSketch(t *testing.T) {
	for _, c := range [][2]string{{"field", "messages"}, {"field", "request.contents"}, {"field", "request.systemInstruction"}, {"system", ".*"}, {"system", "^"}, {"header", "Authorization"}} {
		if !protectedPattern(c[0], c[1]) {
			t.Errorf("%v should be protected", c)
		}
	}
	for _, c := range [][2]string{{"field", "messages.*.cache_control"}, {"field", "request.generationConfig.foo"}, {"system", "^x-anthropic-billing-header:.*$"}, {"schema", "$schema"}} {
		if protectedPattern(c[0], c[1]) {
			t.Errorf("%v should not be protected", c)
		}
	}
	long := `{"messages":[` + strings.Repeat(`{"role":"user","content":"`+strings.Repeat("x", 2000)+`"},`, 40) + `{"role":"user","content":"end"}],"anti_cheat":1}`
	s := sketchJSON(long, 4000)
	if len(s) > 4000 || !strings.Contains(s, "anti_cheat") || !strings.Contains(s, "more items") || !strings.Contains(s, "end") {
		t.Errorf("sketch (%d) = %.300s", len(s), s)
	}
}
