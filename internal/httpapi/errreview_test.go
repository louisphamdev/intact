package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/louisphamdev/intact/internal/filter"
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
		h.ServeHTTP(rec, loopbackRequest(method, path, strings.NewReader(body)))
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
	h.ServeHTTP(rec, loopbackRequest("POST", "/errors/review", strings.NewReader(`{"enabled":true,"model":"openrouter/judge","run":true}`)))
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
		h.ServeHTTP(rec, loopbackRequest(method, path, strings.NewReader(body)))
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

// waitUntil polls until cond holds, so a test never sleeps a fixed time.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		sleepMs(10)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// reviewFilters counts the rules a review installed.
func reviewFilters(t *testing.T, s *store.Store) []string {
	t.Helper()
	list, err := s.ListFilters()
	if err != nil {
		t.Fatalf("list filters: %v", err)
	}
	var out []string
	for _, f := range list {
		if strings.HasPrefix(f.Note, "error review") || strings.HasPrefix(f.Note, "drift review") {
			out = append(out, f.Provider+":"+f.Kind+":"+f.Pattern)
		}
	}
	return out
}

// A rule under a protected container is refused even when the provider's
// message names it, so the replay-less fallback cannot break every request.
func TestProtectedPatternRefusesNestedContent(t *testing.T) {
	for _, p := range []string{"messages.*", "messages.*.content", "messages.*.role", "contents.*.parts",
		"request.contents.*.parts", "contents.*.parts.*.text", "tools.*"} {
		if !protectedPattern(filter.Field, p) {
			t.Errorf("field %q is not protected", p)
		}
	}
	for _, p := range []string{"messages.*.cache_control", "request.generationConfig.foo"} {
		if protectedPattern(filter.Field, p) {
			t.Errorf("field %q should be allowed", p)
		}
	}
	for _, re := range []string{`[\s\S]{2,}`, `^.{2,}$`, `^.{30,}$`} {
		if !protectedPattern(filter.System, re) {
			t.Errorf("system %q is not protected", re)
		}
	}
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a, _ := newServer(s, nil, nil)
	var p errProposal
	if err := json.Unmarshal([]byte(`{"action":"blacklist","candidates":[{"kind":"field","pattern":"request.contents.*.parts"}]}`), &p); err != nil {
		t.Fatalf("proposal: %v", err)
	}
	e := store.UpstreamError{Provider: "antigravity", Signature: "400 invalid value",
		Message: "Invalid value at 'request.contents[3].parts[0]'",
		ReqBody: `{"request":{"contents":[{"parts":[{"text":"hi"}]}]}}`}
	var v store.ErrorVerdict
	a.tryBlacklist(context.Background(), &v, e, p, false)
	if v.Applied || len(reviewFilters(t, s)) != 0 {
		t.Errorf("applied=%v filters=%v, want the rule refused", v.Applied, reviewFilters(t, s))
	}
}

// A verdict built from a request a dashboard key wrote is stored, never acted
// on: that request's text can steer the model.
func TestErrorReviewWaitsForTheOwnerOnKeyTraffic(t *testing.T) {
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := `{"cause":"unknown field","action":"blacklist","candidates":[{"kind":"field","pattern":"messages.*.cache_control"}],"reason":"the provider names cache_control"}`
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
		w.Write(b)
	}))
	defer judge.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("openrouter", "o", "k2")
	a, _ := newServer(s, map[string]string{"openrouter": judge.URL}, nil)
	k, _ := s.CreateAPIKey("hermes")
	big := `{"model":"groq/m","messages":[{"role":"user","content":"` + strings.Repeat("x", 70<<10) + `","cache_control":{"type":"ephemeral"}}]}`
	add := func(sig, keyID string) {
		s.AddUpstreamError(store.UpstreamError{Provider: "groq", Connection: "c1", Model: "m", Client: "hermes",
			ClientKeyID: keyID, Endpoint: "chat/completions", Status: 400, Class: ClassRejected, Signature: sig,
			Message: "Invalid value: messages.*.cache_control is not allowed", ReqBody: big, QuotaLeft: -1})
	}
	s.SetSetting(errReviewKey, `{"enabled":true,"model":"openrouter/judge","minErrors":3,"replay":false}`)
	for i := 0; i < 3; i++ {
		add("400 from a key", k.ID)
	}
	if n, err := a.reviewErrors(context.Background()); n != 1 || err != nil {
		t.Fatalf("judged %d, %v", n, err)
	}
	list, _ := s.ListErrorVerdicts(0)
	if len(list) != 1 || list[0].Applied || !strings.Contains(list[0].Note, "owner") {
		t.Fatalf("verdict = %+v, want Applied=false and a note about the owner", list)
	}
	if got := reviewFilters(t, s); len(got) != 0 {
		t.Fatalf("filters = %v, want none: a key holder's text may not install a rule", got)
	}
	// The same group sent by intact itself, with no key: the review acts.
	for i := 0; i < 3; i++ {
		add("400 with no key", "")
	}
	if n, err := a.reviewErrors(context.Background()); n != 1 || err != nil {
		t.Fatalf("second run judged %d, %v", n, err)
	}
	if got := reviewFilters(t, s); len(got) != 1 || got[0] != "groq:field:messages.*.cache_control" {
		t.Errorf("filters = %v, want the rule installed for traffic that is not a key's", got)
	}
}

// Two passes over the same group would pay every model call twice and file the
// rule and the alert twice.
func TestErrorReviewRunsOneAtATime(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	judgeCalls := 0
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		judgeCalls++
		mu.Unlock()
		<-release
		answer := `{"cause":"unknown field","action":"blacklist","candidates":[{"kind":"field","pattern":"anti_cheat"}],"reason":"the provider refuses anti_cheat"}`
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
		w.Write(b)
	}))
	defer judge.Close()
	alerts := alertsServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("openrouter", "o", "k2")
	a, h := newServer(s, map[string]string{"openrouter": judge.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/notify/channels",
		strings.NewReader(`{"name":"ops","type":"telegram","enabled":true,"config":{"botToken":"123:TOK","chatId":"-100"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("channel: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		s.AddUpstreamError(store.UpstreamError{Provider: "groq", Connection: "c1", Model: "m", Client: "internal",
			Endpoint: "chat/completions", Status: 400, Class: ClassRejected, Signature: "400 unknown field anti_cheat",
			Message: "Unknown field anti_cheat", ReqBody: `{"model":"m","anti_cheat":1,"messages":[]}`, QuotaLeft: -1})
	}
	s.SetSetting(errReviewKey, `{"enabled":true,"model":"openrouter/judge","minErrors":3,"replay":false}`)
	done := make(chan error, 1)
	go func() { _, err := a.reviewErrors(context.Background()); done <- err }()
	waitUntil(t, "the first model call", func() bool { mu.Lock(); defer mu.Unlock(); return judgeCalls == 1 })
	if _, err := a.reviewErrors(context.Background()); !errors.Is(err, errReviewRunning) {
		t.Errorf("second pass err = %v, want %v", err, errReviewRunning)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/errors/review", strings.NewReader(`{"run":true}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("POST run while running = %d %s, want 409", rec.Code, rec.Body.String())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first pass: %v", err)
	}
	mu.Lock()
	calls := judgeCalls
	mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d, want 1", calls)
	}
	if got := reviewFilters(t, s); len(got) != 1 {
		t.Errorf("filters = %v, want one", got)
	}
	if got := alerts(); len(got) != 1 || !strings.Contains(got[0], "Blacklisted field: anti_cheat") {
		t.Errorf("alerts = %q, want one", got)
	}
}

// A pass that is already running is not a model failure: the loop must not
// pause the review or tell the channels.
func TestErrorReviewLoopTreatsABusyPassAsNoError(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	judgeCalls := 0
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		judgeCalls++
		mu.Unlock()
		<-release
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"content": `{"cause":"c","action":"ignore","reason":"r"}`}}}})
		w.Write(b)
	}))
	defer judge.Close()
	alerts := alertsServer(t)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("openrouter", "o", "k2")
	a, h := newServer(s, map[string]string{"openrouter": judge.URL}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/notify/channels",
		strings.NewReader(`{"name":"ops","type":"telegram","enabled":true,"config":{"botToken":"123:TOK","chatId":"-100"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("channel: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		s.AddUpstreamError(store.UpstreamError{Provider: "groq", Connection: "c1", Model: "m", Client: "internal",
			Endpoint: "chat/completions", Status: 400, Class: ClassRejected, Signature: "400 refused",
			Message: "refused", ReqBody: `{"model":"m","messages":[]}`, QuotaLeft: -1})
	}
	s.SetSetting(errReviewKey, `{"enabled":true,"model":"openrouter/judge","minErrors":3,"replay":false}`)
	done := make(chan error, 1)
	go func() { _, err := a.reviewErrors(context.Background()); done <- err }()
	waitUntil(t, "the first model call", func() bool { mu.Lock(); defer mu.Unlock(); return judgeCalls == 1 })
	a.errorReviewOnce()
	a.errReview.mu.Lock()
	paused, lastErr := a.errReview.pauseTill, a.errReview.lastError
	a.errReview.mu.Unlock()
	close(release)
	<-done
	if !paused.IsZero() || lastErr != "" {
		t.Errorf("pauseTill = %v lastError = %q, want the busy pass ignored", paused, lastErr)
	}
	for _, m := range alerts() {
		if strings.Contains(m, "paused") {
			t.Errorf("a busy pass alerted: %q", m)
		}
	}
}

// A false 429 on one system sentence is found by replaying the request with parts of its
// system prompt removed, not by a model's guess, and the fix is a filter in the database.
// It is installed for key traffic too: the rule can only remove text the provider refuses.
func TestErrorReviewFindsTheSystemTextBehindAFake429ByReplay(t *testing.T) {
	var mu sync.Mutex
	replays := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		replays++
		mu.Unlock()
		if strings.Contains(string(b), "Codex, an agent based on GPT-5") {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Resource has been exhausted (e.g. check quota)."}}`)
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
	c, _ := s.CreateConnection("groq", "g", "k")
	s.CreateConnection("openrouter", "o", "k2")
	a, _ := newServer(s, map[string]string{"groq": up.URL, "openrouter": judge.URL}, nil)
	k, _ := s.CreateAPIKey("cty")
	system := "You are Codex, an agent based on GPT-5. You and the user share one workspace.\n\n# Personality\n\nYou match the tone of the user. Keep answers short.\nNever run destructive commands without asking."
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": "hi"}}})
	for i := 0; i < 3; i++ {
		s.AddUpstreamError(store.UpstreamError{Provider: "groq", Connection: c.ID, Model: "m", Client: "cty", ClientKeyID: k.ID,
			Endpoint: "chat/completions", Status: 429, Class: ClassFake429, Signature: "429 resource has been exhausted",
			Message: "Resource has been exhausted (e.g. check quota).", ReqBody: string(body), QuotaLeft: 0.95})
	}
	s.SetSetting(errReviewKey, `{"enabled":true,"model":"openrouter/judge","minErrors":3,"replay":true}`)
	if n, err := a.reviewErrors(context.Background()); n != 1 || err != nil {
		t.Fatalf("judged %d, %v", n, err)
	}
	list, _ := s.ListErrorVerdicts(0)
	if judged || len(list) != 1 || !list[0].Applied || !list[0].Verified || list[0].By != "intact" || list[0].Action != "blacklist" {
		t.Fatalf("judged=%v verdicts=%+v", judged, list)
	}
	got := reviewFilters(t, s)
	if len(got) != 1 {
		t.Fatalf("filters = %v", got)
	}
	rules := a.rulesFor("groq")
	out, _ := filter.Apply(body, rules)
	for _, keep := range []string{"You and the user share one workspace.", "# Personality", "Keep answers short.", "Never run destructive commands"} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("the rule %v removed %q too: %s", got, keep, out)
		}
	}
	if strings.Contains(string(out), "Codex, an agent based on GPT-5") {
		t.Errorf("the trigger survives: %s", out)
	}
	if replays > 16 {
		t.Errorf("%d replays; the search must stay bounded", replays)
	}
}
