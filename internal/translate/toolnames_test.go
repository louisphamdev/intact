package translate

import (
	"bytes"
	"strings"
	"testing"
)

// A model answers with the tool names it was trained on, whatever the caller declared. intact
// translates the names on the way to the provider and back on the way out, so the caller receives a
// call it can run. Sources for every name are in docs/tool-vocabulary.md.

func TestACallerToolGoesOutUnderTheProviderOwnName(t *testing.T) {
	names := NewToolNames([]string{"Read", "Bash", "Edit", "Grep", "Glob"}, "codex")
	if got := names.ToProvider("Bash"); got != "shell" {
		t.Errorf("Bash went out as %q, want shell", got)
	}
	if got := names.ToProvider("Read"); got != "read_file" {
		t.Errorf("Read went out as %q, want read_file", got)
	}
	if got := names.ToProvider("Grep"); got != "rg" {
		t.Errorf("Grep went out as %q, want rg", got)
	}
	if got := names.ToProvider("Glob"); got != "glob_file_search" {
		t.Errorf("Glob went out as %q, want glob_file_search", got)
	}
	// Codex has no separate write tool, so an edit is not offered under a name that would change what
	// the tool does: apply_patch speaks a diff, Edit speaks old_string/new_string.
	if got := names.ToProvider("Edit"); got != "Edit" {
		t.Errorf("Edit went out as %q, want it unchanged", got)
	}
}

func TestTheAnswerComesBackInTheCallerOwnName(t *testing.T) {
	names := NewToolNames([]string{"Read", "Bash", "Edit"}, "codex")
	if got := names.ToClient("shell"); got != "Bash" {
		t.Errorf("shell came back as %q, want Bash", got)
	}
	if got := names.ToClient("read_file"); got != "Read" {
		t.Errorf("read_file came back as %q, want Read", got)
	}
	// A name the caller declared is its own, and so is one that says nothing.
	for _, own := range []string{"Read", "Bash", "somethingelse"} {
		if got := names.ToClient(own); got != own {
			t.Errorf("%q came back as %q", own, got)
		}
	}
}

func TestAGeminiAccountGetsTheGeminiNames(t *testing.T) {
	names := NewToolNames([]string{"Read", "Write", "Edit", "Grep"}, "antigravity")
	// A name the caller never declared is never turned into one it did: there is no such tool to run.
	if got := names.ToProvider("Bash"); got != "Bash" {
		t.Errorf("an undeclared Bash went out as %q", got)
	}
	if got := names.ToProvider("Read"); got != "read_file" {
		t.Errorf("Read went out as %q, want read_file", got)
	}
	if got := names.ToProvider("Write"); got != "write_file" {
		t.Errorf("Write went out as %q, want write_file", got)
	}
	if got := names.ToProvider("Edit"); got != "replace" {
		t.Errorf("Edit went out as %q, want replace", got)
	}
	if got := names.ToClient("replace"); got != "Edit" {
		t.Errorf("replace came back as %q, want Edit", got)
	}
}

// An account whose models take the names the caller sent has nothing to prefer, so the name it is given
// does not change. The answer still has to come back: a model reaches for its own vocabulary whichever
// endpoint serves it, so an OpenAI-compatible account behind a Claude Code caller answers `shell`, and
// a caller that declared Bash refuses a name it does not have.
func TestAProviderWithNoVocabularyKeepsTheNameItIsGivenAndStillMatchesTheAnswer(t *testing.T) {
	for _, provider := range []string{"groq", "bifrost", "openrouter", "claude"} {
		names := NewToolNames([]string{"Read", "Bash"}, provider)
		if got := names.ToProvider("Bash"); got != "Bash" {
			t.Errorf("%s: Bash went out as %q, want Bash", provider, got)
		}
		if got := names.ToClient("shell"); got != "Bash" {
			t.Errorf("%s: shell came back as %q, want Bash", provider, got)
		}
		if got := names.ToClient("read"); got != "Read" {
			t.Errorf("%s: read came back as %q, want Read", provider, got)
		}
	}
}

// The same boundary holds where the caller declares one tool: a single name has no second answer to
// pick between, so an invented one stays as the model wrote it.
func TestASingleDeclaredToolIsNotRenamed(t *testing.T) {
	names := NewToolNames([]string{"Bash"}, "bifrost")
	if got := names.ToClient("shell"); got != "shell" {
		t.Errorf("shell came back as %q with one tool declared, want shell", got)
	}
}

// Claude Code on Windows declares both Bash and PowerShell. Which one the model meant is not knowable,
// and the wrong one runs, so neither moves.
func TestTwoToolsOfTheSameKindAreNotTranslated(t *testing.T) {
	names := NewToolNames([]string{"Bash", "PowerShell", "Read"}, "codex")
	if got := names.ToProvider("Bash"); got != "Bash" {
		t.Errorf("Bash went out as %q with PowerShell also declared", got)
	}
	if got := names.ToProvider("PowerShell"); got != "PowerShell" {
		t.Errorf("PowerShell went out as %q with Bash also declared", got)
	}
	if got := names.ToProvider("Read"); got != "read_file" {
		t.Errorf("the unambiguous one did not move: %q", got)
	}
}

// Renaming mcp__github__create_issue into a local Write produces a call the caller cannot detect as
// wrong: it writes a file instead of opening an issue, and it succeeds.
func TestANamespacedToolNeverLeavesItsNamespace(t *testing.T) {
	names := NewToolNames([]string{"Write", "Read", "mcp__github__create_issue"}, "codex")
	if got := names.ToProvider("mcp__github__create_issue"); got != "mcp__github__create_issue" {
		t.Errorf("a namespaced tool was sent as %q", got)
	}
	if got := names.ToClient("mcp__github__create_issue"); got != "mcp__github__create_issue" {
		t.Errorf("a namespaced tool came back as %q", got)
	}
}

// A server tool carries a dated type and no input_schema. It is not the caller's own tool, it is not
// translated, and it is not offered as a rename target.
func TestAServerToolIsNeitherTranslatedNorOffered(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tools":[
		{"name":"Read","input_schema":{"type":"object","properties":{}}},
		{"name":"Bash","input_schema":{"type":"object","properties":{}}},
		{"type":"web_search_20260209","name":"web_search"}]}`)
	declared := AnthropicToolNames(body)
	if len(declared) != 2 {
		t.Fatalf("the declared tools are %v, want Read and Bash only", declared)
	}
	names := NewToolNames(declared, "codex")
	if got := names.ToClient("web_search"); got != "web_search" {
		t.Errorf("a server tool became %q", got)
	}
}

// The whole request: the list and the transcript go out under the provider's names, and the call comes
// back under the caller's. A tools array that says read_file while the transcript says Read reads to
// the model as two different tools, which is worse than having changed nothing.
func TestTheToolsAndTheTranscriptMoveTogether(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}},{"name":"Bash","input_schema":{"type":"object"}}]}`)
	names := NewToolNames(AnthropicToolNames(body), "codex")

	out, err := AnthropicToOpenAI(body, names)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)

	declared := map[string]bool{}
	for _, raw := range list(m["tools"]) {
		declared[str(asObj(asObj(raw)["function"])["name"])] = true
	}
	if !declared["shell"] || !declared["read_file"] {
		t.Errorf("the provider was sent %v, want shell and read_file", declared)
	}

	found := false
	for _, raw := range list(m["messages"]) {
		msg := asObj(raw)
		if str(msg["role"]) != "assistant" {
			continue
		}
		for _, rawCall := range list(msg["tool_calls"]) {
			call := asObj(rawCall)
			if str(asObj(call["function"])["name"]) == "shell" {
				found = true
			}
			if got := str(asObj(call["function"])["arguments"]); got != `{"command":"ls"}` {
				t.Errorf("the arguments were rewritten: %s", got)
			}
		}
	}
	if !found {
		t.Errorf("the transcript still says Bash: %s", j(m["messages"]))
	}
}

// The reply the caller reads: the provider's name comes back as the caller's, and the arguments are
// untouched, because a renamed tool keeps the caller's schema and none of its keys move.
func TestTheAnswerComesBackWithTheCallersNameAndItsOwnArguments(t *testing.T) {
	names := NewToolNames([]string{"Read", "Bash"}, "codex")
	r := Reply{Tools: names}

	up, err := OpenAIResponseToAnthropic([]byte(`{"id":"1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls",
		"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]}}]}`), r)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, up)
	block := asObj(list(m["content"])[0])
	if str(block["name"]) != "Bash" {
		t.Errorf("the call came back as %q, want Bash", str(block["name"]))
	}
	if got := j(block["input"]); got != `{"command":"ls"}` {
		t.Errorf("the arguments were rewritten: %s", got)
	}
}

// With no translation the reply is exactly what it was, which is what every provider without a
// vocabulary of its own gets.
func TestARequestWithNoTranslationIsUntouched(t *testing.T) {
	names := NewToolNames(nil, "codex")
	r := Reply{}
	if got := r.callerName("shell"); got != "shell" {
		t.Errorf("a nil translation changed a name to %q", got)
	}
	if got := names.ToProvider("Bash"); got != "Bash" {
		t.Errorf("no declared tools produced %q", got)
	}
	var nilNames *ToolNames
	if got := nilNames.ToClient("shell"); got != "shell" {
		t.Errorf("a nil ToolNames changed a name to %q", got)
	}
}

func TestANameTheModelInventedIsLeftAlone(t *testing.T) {
	names := NewToolNames([]string{"Read", "Bash", "Edit"}, "codex")
	for _, unknown := range []string{"deploy", "mystery_tool", "do_something"} {
		if got := names.ToClient(unknown); got != unknown {
			t.Errorf("%q came back as %q", unknown, got)
		}
	}
}

// A Gemini account: the declaration goes out under the name Gemini is trained on, and the call comes
// back under the caller's. `replace` requires an `instruction` that a caller's Edit cannot produce, so
// the caller's own schema travels with the renamed declaration and nothing is required of it.
func TestAGeminiDeclarationAndAnswer(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[],"tools":[
		{"type":"function","function":{"name":"Read","description":"mine","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}},
		{"type":"function","function":{"name":"Edit","description":"mine","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}}}}}]}`)
	names := NewToolNames([]string{"Read", "Edit"}, "antigravity")

	inner, err := OpenAIToGemini(chat, nil, names)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, inner)
	decl := map[string]bool{}
	for _, raw := range list(m["tools"]) {
		for _, d := range list(asObj(raw)["functionDeclarations"]) {
			d := asObj(d)
			decl[str(d["name"])] = true
			if s := str(d["description"]); s != "mine" {
				t.Errorf("the description was rewritten: %q", s)
			}
		}
	}
	if !decl["read_file"] || !decl["replace"] {
		t.Errorf("Gemini was offered %v, want read_file and replace", decl)
	}

	// The stream answers with the name it was offered; the caller gets its own.
	src := `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"replace","args":{"file_path":"a.ts","old_string":"x","new_string":"y"}}}]},"finishReason":"STOP"}]}}` + "\n\n"
	var buf bytes.Buffer
	GeminiStreamToOpenAI(capFlush{&buf}, strings.NewReader(src), nil, names)
	if !strings.Contains(buf.String(), `"name":"Edit"`) {
		t.Errorf("the call came back without the caller's name: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `\"old_string\":\"x\"`) {
		t.Errorf("the arguments were rewritten: %s", buf.String())
	}
}

// A Codex client sends its own vocabulary already, so against a Codex account nothing moves: a rename
// there would only rename a name the caller and the model both know.
func TestACodexClientKeepsItsOwnNames(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"go","tools":[
		{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}},
		{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
	names := NewToolNames(ResponsesToolNames(body), "codex")

	out, err := ResponsesToOpenAI(body, names)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	got := map[string]bool{}
	for _, raw := range list(m["tools"]) {
		got[str(asObj(asObj(raw)["function"])["name"])] = true
	}
	if !got["shell"] || !got["read_file"] {
		t.Errorf("a Codex client was sent %v, want its own names", got)
	}

	tools := ResponsesTools(body, names)
	for _, want := range []string{"shell", "read_file"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("the answer map has no %q; got %v", want, tools)
		}
	}
}

// A custom tool carries a grammar written for its name, and local_shell is the harness's own. Neither
// is renamed: a renamed freeform tool would arrive with a grammar the model was never given.
func TestACustomToolAndLocalShellAreNeverRenamed(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"go","tools":[
		{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},
		{"type":"local_shell"},
		{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)
	names := NewToolNames(ResponsesToolNames(body), "codex")
	// local_shell is not offered for translation, so it cannot be a rename target either.
	if got := names.ToProvider("local_shell"); got != "local_shell" {
		t.Errorf("local_shell was renamed to %q", got)
	}

	out, err := ResponsesToOpenAI(body, names)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	got := map[string]bool{}
	for _, raw := range list(m["tools"]) {
		got[str(asObj(asObj(raw)["function"])["name"])] = true
	}
	if !got["apply_patch"] || !got["local_shell"] {
		t.Errorf("a freeform tool or the harness tool was renamed: %v", got)
	}
	tools := ResponsesTools(body, names)
	if _, ok := tools["local_shell"]; !ok {
		t.Errorf("the answer map lost local_shell: %v", tools)
	}
}
