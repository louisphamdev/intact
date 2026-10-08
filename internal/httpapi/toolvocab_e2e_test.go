package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// An OpenAI-compatible account answers in the vocabulary it was trained on, whichever endpoint
// serves it: `shell`, not the caller's `Bash`. The caller refuses a name it never declared
// ("No such tool available: shell"), so intact has to write the call back in the caller's own name.
// This ran through every provider but codex and gemini, which have a vocabulary table of their own.
func TestAModelAnsweringInItsOwnVocabularyComesBackInTheCallers(t *testing.T) {
	var upstream []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools []struct {
				Name string `json:"name"`
				Fn   struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.Unmarshal(raw, &body)
		for _, tl := range body.Tools {
			if tl.Name != "" {
				upstream = append(upstream, tl.Name)
			} else if tl.Fn.Name != "" {
				upstream = append(upstream, tl.Fn.Name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		// The name the model reaches for on its own, deliberately not the caller's.
		w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"openrouter/vendor/m","choices":[{"index":0,
			"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":[\"ls\"]}"}}]},
			"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("openrouter", "acc", "key")
	s.SetBaseURL(c.ID, up.URL+"/api/v1")
	h := New(s, nil)

	body := `{"model":"openrouter/vendor/m","max_tokens":64,
		"messages":[{"role":"user","content":"list the files"}],
		"tools":[
			{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}},
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Nothing is renamed on the way out: this provider has no vocabulary of its own, so the caller's
	// own names are as good as any.
	if strings.Join(upstream, ",") != "Bash,Read" {
		t.Errorf("the names went upstream as %v, want Bash and Read unchanged", upstream)
	}
	var reply struct {
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Command []string `json:"command"`
			} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply is not Anthropic: %v (%s)", err, rec.Body.String())
	}
	if len(reply.Content) != 1 || reply.Content[0].Type != "tool_use" {
		t.Fatalf("content = %s", rec.Body.String())
	}
	if reply.Content[0].Name != "Bash" {
		t.Errorf("the call came back as %q, want Bash", reply.Content[0].Name)
	}
	if len(reply.Content[0].Input.Command) != 1 || reply.Content[0].Input.Command[0] != "ls" {
		t.Errorf("the arguments did not survive: %s", rec.Body.String())
	}
	if reply.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", reply.StopReason)
	}
}

// The same boundary on the way a Claude Code caller actually arrives: OpenAI chat in, a tool result
// back in, and the second answer still written in the caller's own name.
func TestAToolResultRoundTripKeepsTheCallersNames(t *testing.T) {
	var names []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &body)
		for _, m := range body.Messages {
			if m.Role == "tool" {
				names = append(names, m.ToolCallID)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c2","object":"chat.completion","model":"openrouter/vendor/m","choices":[{"index":0,
			"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("openrouter", "acc", "key")
	s.SetBaseURL(c.ID, up.URL+"/api/v1")
	h := New(s, nil)

	body := `{"model":"openrouter/vendor/m","max_tokens":64,
		"messages":[
			{"role":"user","content":"list the files"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":[\"ls\"]}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"README.md"}],
		"tools":[
			{"type":"function","function":{"name":"Bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}},
			{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(names) != 1 || names[0] != "call_1" {
		t.Errorf("the tool result did not arrive paired with its call: %v", names)
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("reply = %s", rec.Body.String())
	}
}
