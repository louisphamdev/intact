package httpapi

import (
	"bytes"
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

// agModelsFixture is a trimmed fetchAvailableModels answer captured from a real
// account; the values the envelope copies are kept as Google sent them.
const agModelsFixture = `{"models":{
"gemini-3.8-flash-high":{"supportsThinking":true,"thinkingBudget":-1,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M318","modelProvider":"MODEL_PROVIDER_GOOGLE"},
"gemini-3.8-flash-medium":{"supportsThinking":true,"thinkingBudget":4000,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M319","modelProvider":"MODEL_PROVIDER_GOOGLE"},
"gemini-3.8-flash-low":{"supportsThinking":true,"thinkingBudget":1000,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M320","modelProvider":"MODEL_PROVIDER_GOOGLE"},
"claude-sonnet-4-6":{"supportsThinking":true,"thinkingBudget":1024,"maxOutputTokens":64000,"model":"MODEL_PLACEHOLDER_M35","modelProvider":"MODEL_PROVIDER_ANTHROPIC"}}}`

// agToolTurn is the tool round trip of the captured agy session: a request, a
// call, and the call's result.
const agToolTurn = `"messages":[
{"role":"system","content":"sys"},
{"role":"user","content":"Read note.txt"},
{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"view_file","arguments":"{\"AbsolutePath\":\"/tmp/note.txt\"}"}}]},
{"role":"tool","tool_call_id":"call_1","content":"line-one-marker"}],
"tools":[{"type":"function","function":{"name":"view_file","description":"View a file.","parameters":{"type":"object","properties":{"AbsolutePath":{"type":"string"}},"required":["AbsolutePath"]}}}]`

type agCapture struct {
	mu        sync.Mutex
	chat      []string
	chatUA    []string
	loadPath  string
	loadBody  string
	listHeads http.Header
}

// agHarness serves the Code Assist calls intact makes and records them.
func agHarness(t *testing.T) (http.Handler, *agCapture) {
	t.Helper()
	got := &agCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		defer got.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			got.loadPath, got.loadBody = r.URL.Path, string(b)
			w.Write([]byte(`{"cloudaicompanionProject":"aicode-consumers"}`))
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			got.listHeads = r.Header.Clone()
			w.Write([]byte(agModelsFixture))
		default:
			got.chat = append(got.chat, string(b))
			got.chatUA = append(got.chatUA, r.Header.Get("User-Agent"))
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}}`+"\n\n")
		}
	}))
	t.Cleanup(up.Close)
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("antigravity", "ag", "fake-access-token")
	return New(s, map[string]string{"antigravity": up.URL}), got
}

func (c *agCapture) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.chat) == 0 {
		t.Fatal("no chat request reached the upstream")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.chat[len(c.chat)-1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (c *agCapture) lastRaw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chat[len(c.chat)-1]
}

// keyOrder lists the keys of a JSON object in the order they were written.
func keyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	if tok, _ := d.Token(); tok != json.Delim('{') {
		t.Fatalf("not an object: %.60s", raw)
	}
	var keys []string
	for d.More() {
		k, _ := d.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		d.Decode(&skip)
	}
	return keys
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAntigravityEnvelopeMatchesTheCLI(t *testing.T) {
	h, got := agHarness(t)
	rec := postV1(h, `{"model":"antigravity/gemini-3.8-flash","reasoning_effort":"high",`+agToolTurn+`}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	env := got.last(t)
	raw := []byte(got.lastRaw())
	if k := strings.Join(keyOrder(t, raw), ","); k != "project,requestId,request,model,userAgent,requestType" {
		t.Errorf("envelope key order = %s", k)
	}
	inner, _ := json.Marshal(env["request"])
	var innerRaw struct {
		Request json.RawMessage `json:"request"`
	}
	json.Unmarshal(raw, &innerRaw)
	if k := strings.Join(keyOrder(t, innerRaw.Request), ","); k != "contents,systemInstruction,tools,labels,generationConfig,sessionId" {
		t.Errorf("request key order = %s", k)
	}
	req := env["request"].(map[string]any)
	if g := mustJSON(req["generationConfig"]); g != `{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}}` {
		t.Errorf("generationConfig = %s", g)
	}
	if _, ok := req["toolConfig"]; ok {
		t.Errorf("toolConfig sent although the client named no tool choice: %s", inner)
	}
	if !strings.Contains(mustJSON(req["tools"]), `"type":"OBJECT"`) || !strings.Contains(mustJSON(req["tools"]), `"type":"STRING"`) {
		t.Errorf("schema types are not upper case: %s", mustJSON(req["tools"]))
	}
	var roles []string
	for _, c := range req["contents"].([]any) {
		roles = append(roles, c.(map[string]any)["role"].(string))
	}
	if r := strings.Join(roles, ","); r != "user,model,model" {
		t.Errorf("roles = %s", r)
	}
	if !strings.Contains(mustJSON(req["contents"]), `"response":{"output":"line-one-marker"}`) {
		t.Errorf("function response = %s", mustJSON(req["contents"]))
	}
	labels := req["labels"].(map[string]any)
	traj, _ := labels["trajectory_id"].(string)
	want := map[string]string{"last_step_index": "2", "model_enum": "MODEL_PLACEHOLDER_M318", "request_id": traj + "-1",
		"used_claude": "false", "used_claude_conservative": "false", "used_non_gemini_model": "false"}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("labels[%s] = %v, want %s", k, labels[k], v)
		}
	}
	if len(traj) != 36 {
		t.Errorf("trajectory_id = %q", traj)
	}
	if id, _ := env["requestId"].(string); !strings.HasPrefix(id, "agent/") || !strings.HasSuffix(id, "/"+traj+"/3") {
		t.Errorf("requestId = %s", id)
	}
	if env["model"] != "gemini-3.8-flash-high" {
		t.Errorf("model = %v", env["model"])
	}
	if ua := got.chatUA[0]; !strings.HasPrefix(ua, "antigravity/cli/") {
		t.Errorf("chat User-Agent = %s", ua)
	}
}

func TestAntigravityClaudeModelUsesItsOwnBudgetAndRoles(t *testing.T) {
	h, got := agHarness(t)
	postV1(h, `{"model":"antigravity/claude-sonnet-4-6",`+agToolTurn+`}`)
	req := got.last(t)["request"].(map[string]any)
	if g := mustJSON(req["generationConfig"]); g != `{"maxOutputTokens":64000,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1024}}` {
		t.Errorf("generationConfig = %s", g)
	}
	var roles []string
	for _, c := range req["contents"].([]any) {
		roles = append(roles, c.(map[string]any)["role"].(string))
	}
	if r := strings.Join(roles, ","); r != "user,model,user" {
		t.Errorf("roles = %s", r)
	}
	labels := req["labels"].(map[string]any)
	if labels["model_enum"] != "MODEL_PLACEHOLDER_M35" || labels["used_claude"] != "true" || labels["used_non_gemini_model"] != "true" {
		t.Errorf("labels = %v", labels)
	}
}

func TestAntigravityLowEffortTakesTheLowVariantBudget(t *testing.T) {
	h, got := agHarness(t)
	postV1(h, `{"model":"antigravity/gemini-3.8-flash","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	env := got.last(t)
	if env["model"] != "gemini-3.8-flash-low" {
		t.Errorf("model = %v", env["model"])
	}
	if g := mustJSON(env["request"].(map[string]any)["generationConfig"]); g != `{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1000}}` {
		t.Errorf("generationConfig = %s", g)
	}
}

func TestAntigravityTrajectoryFollowsTheConversation(t *testing.T) {
	h, got := agHarness(t)
	traj := func() string {
		return got.last(t)["request"].(map[string]any)["labels"].(map[string]any)["trajectory_id"].(string)
	}
	postV1(h, `{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"Read note.txt"}]}`)
	first := traj()
	postV1(h, `{"model":"antigravity/gemini-3.8-flash",`+agToolTurn+`}`)
	if traj() != first {
		t.Errorf("a later step of the same conversation changed trajectory: %s vs %s", traj(), first)
	}
	postV1(h, `{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"Something else"}]}`)
	if traj() == first {
		t.Errorf("another conversation kept trajectory %s", first)
	}
}

func TestAntigravitySideCallsLookLikeTheCLI(t *testing.T) {
	h, got := agHarness(t)
	postV1(h, `{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.loadPath != "/v1internal:loadCodeAssist" || got.loadBody != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Errorf("loadCodeAssist went to %q with %s", got.loadPath, got.loadBody)
	}
	if got.listHeads == nil {
		t.Fatal("fetchAvailableModels was not called")
	}
	if ua := got.listHeads.Get("User-Agent"); !strings.HasPrefix(ua, "antigravity/cli/") {
		t.Errorf("fetchAvailableModels User-Agent = %s", ua)
	}
	for _, h := range []string{"X-Client-Name", "X-Client-Version"} {
		if v := got.listHeads.Get(h); v != "" {
			t.Errorf("fetchAvailableModels sent %s: %s; the CLI sends none", h, v)
		}
	}
}
