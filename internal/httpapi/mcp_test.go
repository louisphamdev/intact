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

func rpc(t *testing.T, h http.Handler, token, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusAccepted {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	return m
}

func toolText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	res := m["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), isErr
}

func TestMCPManagesFilters(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	tok := cfg.APIToken

	init := rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "intact" {
		t.Fatalf("initialize = %v", init)
	}
	if rpc(t, h, tok, `{"jsonrpc":"2.0","method":"notifications/initialized"}`) != nil {
		t.Error("a notification got an answer")
	}
	list := rpc(t, h, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := []string{}
	for _, x := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if !strings.Contains(strings.Join(names, ","), "add_filter") {
		t.Fatalf("tools = %v", names)
	}

	add := rpc(t, h, tok, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add_filter","arguments":{"provider":"groq","kind":"field","pattern":"reasoning_effort","note":"groq rejects it"}}}`)
	text, isErr := toolText(t, add)
	if isErr || !strings.Contains(text, `"reasoning_effort"`) {
		t.Fatalf("add_filter = %s", text)
	}
	var saved store.Filter
	json.Unmarshal([]byte(text), &saved)

	got := rpc(t, h, tok, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_filters","arguments":{"provider":"groq"}}}`)
	if text, _ := toolText(t, got); !strings.Contains(text, saved.ID) {
		t.Errorf("list_filters = %s", text)
	}
	bad := rpc(t, h, tok, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"add_filter","arguments":{"kind":"system","pattern":"("}}}`)
	if _, isErr := toolText(t, bad); !isErr {
		t.Error("a bad regex was not reported as a tool error")
	}
	del := rpc(t, h, tok, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"delete_filter","arguments":{"id":"`+saved.ID+`"}}}`)
	if _, isErr := toolText(t, del); isErr {
		t.Error("delete_filter failed")
	}
	if m := rpc(t, h, tok, `{"jsonrpc":"2.0","id":7,"method":"nope"}`); m["error"] == nil {
		t.Error("unknown method did not return an error")
	}
}

func TestManagementAPINeedsTheToken(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	for _, path := range []string{"/api/filters", "/api/accounts", "/api/providers", "/api/usage"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: code=%d", path, rec.Code)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with token: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest("POST", "/api/filters", strings.NewReader(`{"kind":"header","pattern":"x-debug"}`))
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var f store.Filter
	json.Unmarshal(rec.Body.Bytes(), &f)
	if rec.Code != http.StatusOK || f.Pattern != "X-Debug" || f.Provider != "*" {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("DELETE", "/api/filters/"+f.ID, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("delete: code=%d", rec.Code)
	}
}
