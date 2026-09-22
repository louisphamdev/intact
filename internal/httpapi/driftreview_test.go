package httpapi

import (
	"context"
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

// fakeJev answers the review question with the cause and confidence the test
// sets per path, and records the states it was sent.
func fakeJev(verdicts map[string][2]any) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var states []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ State string }
		json.NewDecoder(r.Body).Decode(&b)
		var st map[string]any
		json.Unmarshal([]byte(b.State), &st)
		mu.Lock()
		states = append(states, st)
		mu.Unlock()
		v := verdicts[st["path"].(string)]
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"cause": map[string]any{"type": "choice", "choice": v[0], "confidence": v[1], "probabilities": map[string]float64{}}}})
	}))
	return up, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return states }
}

func TestDriftReviewAcksOnlyConfidentBenignChanges(t *testing.T) {
	up, states := fakeJev(map[string][2]any{
		"stream":                    {CauseNewClient, 0.9},
		"choices[].delta":           {CauseProviderChange, 0.8},
		"tools":                     {CauseNewClient, 0.4},
		"messages[].tool_calls":     {CauseNewClient, 0.9},
		"response.parts[].args.{*}": {CauseDataNoise, 0.7},
	})
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("typesafe", "jev", "k")
	a, h := newServer(s, map[string]string{"typesafe": up.URL}, nil)
	add := func(prov, dir, path, kind, client string) {
		s.AddShapeChange(store.ShapeChange{Direction: dir, Provider: prov, Endpoint: "chat/completions", Path: path, Kind: kind, Client: client})
	}
	add("codex", "request", "stream", "added", "hermes")
	add("codex", "response", "choices[].delta", "added", "")
	add("codex", "request", "tools", "added", "hermes")
	add("github", "request", "messages[].tool_calls", "added", "hermes")
	add("codex", "response", "response.parts[].args.{*}", "added", "")
	// github answered with an error after its change: no automatic ack.
	a.errs.note("github")
	n, err := a.reviewPending(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("reviewed %d, %v", n, err)
	}
	list, _ := s.ListShapeChanges(store.ShapeChangeFilter{})
	got := map[string]string{}
	for _, c := range list {
		state := "kept"
		if c.Acked && c.AutoAcked {
			state = "acked"
		}
		got[c.Path] = c.Verdict + " " + state
	}
	want := map[string]string{
		"stream":                    CauseNewClient + " acked",
		"choices[].delta":           CauseProviderChange + " kept",
		"tools":                     CauseNewClient + " kept",
		"messages[].tool_calls":     CauseNewClient + " kept",
		"response.parts[].args.{*}": CauseDataNoise + " acked",
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %q, want %q", p, got[p], w)
		}
	}
	// The facts intact sends.
	for _, st := range states() {
		f := st["facts"].(map[string]any)
		switch st["path"] {
		case "response.parts[].args.{*}":
			if f["inside_tool_call_arguments"] != true {
				t.Errorf("args facts = %v", f)
			}
		case "choices[].delta":
			if f["streamed_answer"] != true || f["a_client_started_streaming_or_tools_this_hour"] != true {
				t.Errorf("delta facts = %v", f)
			}
		case "messages[].tool_calls":
			if f["failed_answers_since"].(float64) != 1 || f["client"] != "hermes" {
				t.Errorf("github facts = %v", f)
			}
		}
	}
	// Nothing left to judge; switched off, nothing is judged.
	if n, _ := a.reviewPending(context.Background()); n != 0 {
		t.Errorf("judged again: %d", n)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/drift/review", strings.NewReader(`{"enabled":false}`)))
	add("codex", "request", "stream_options", "added", "hermes")
	if n, _ := a.reviewPending(context.Background()); n != 0 {
		t.Errorf("judged while off: %d", n)
	}
}

func TestDriftLearnsClientsApartAndSkipsArguments(t *testing.T) {
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"x","tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}}],"meta":{"args":{"deep":{"x":1}}}}`))
	}))
	defer up.Close()
	_ = body
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("github", "g", "k")
	kA, _ := s.CreateAPIKey("alpha")
	kB, _ := s.CreateAPIKey("beta")
	fa, _ := s.RevealAPIKey(kA.ID)
	fb, _ := s.RevealAPIKey(kB.ID)
	a, h := newServer(s, map[string]string{"github": up.URL}, nil)
	call := func(key, b string) {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	// alpha always sends max_tokens, beta never: learned apart, neither is a change.
	for i := 0; i < 30; i++ {
		call(fa, `{"model":"github/gpt-4.1","max_tokens":5,"messages":[]}`)
		call(fb, `{"model":"github/gpt-4.1","messages":[]}`)
	}
	// Let the background observer finish.
	for i := 0; i < 50; i++ {
		a.drift.Drain()
		sleepMs(20)
	}
	list, _ := s.ListShapeChanges(store.ShapeChangeFilter{})
	for _, c := range list {
		if c.Direction == "request" {
			t.Errorf("request change from mixed clients: %+v", c)
		}
	}
	fields := a.drift.Fields("request", "github", "")
	keys := map[string]bool{}
	for _, f := range fields {
		keys[f.Key] = true
		if strings.Contains(f.Path, "deep") {
			t.Errorf("learned inside arguments: %s", f.Path)
		}
	}
	if !keys["request|github|chat/completions@alpha|"] || !keys["request|github|chat/completions@beta|"] {
		t.Errorf("keys = %v", keys)
	}
	for _, f := range a.drift.Fields("response", "github", "") {
		if strings.Contains(f.Path, "deep") || strings.Contains(f.Path, ".x") {
			t.Errorf("learned inside args: %s", f.Path)
		}
	}
}
