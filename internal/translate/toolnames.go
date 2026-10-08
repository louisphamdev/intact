package translate

import "strings"

// Tool names, and who runs the tool.
//
// A tool name is not an identifier an API checks. A caller declares its tools, the model answers with
// a name, and the caller runs the call or refuses it ("No such tool available: read"). The model
// reaches for the vocabulary it was trained on, which is not the caller's: a Codex model answers
// `shell` and `read_file` whatever the caller declared, because those are the names its own prompt
// names (openai/codex, codex-rs/core/prompt_with_apply_patch_instructions.md: "Use the apply_patch
// shell command to edit files").
//
// So a name is translated on the way to the provider and translated back on the way out. The map is
// built on the way in, which is what makes the way back exact: the name the model answers with is a
// name intact sent, so the answer is a lookup and not a guess.
//
// Two boundaries hold whatever the name looks like.
//
// Who runs the tool decides whether a name may move at all. A client tool runs in the caller's
// application; a provider tool runs on the provider's servers and its result reaches the caller with
// nothing executed locally (Anthropic: "Server tools such as web_search, web_fetch, code_execution
// and tool_search run on Anthropic's infrastructure"). Renaming one into the other changes who does
// the work, silently: a server-side search becomes a local tool call, and nothing reports it.
//
// Two plausible targets mean no change. Claude Code on macOS ships without Glob and Grep and searches
// through Bash instead (code.claude.com/docs/en/tools.md), and a Windows session declares both Bash
// and PowerShell. When a name could be either it stands, and the caller reports the mismatch itself: a
// refused call is visible, a wrongly rewritten one runs.
//
// A renamed tool keeps the caller's schema. opencode spells a file argument filePath and Claude Code
// spells it file_path, so translating argument keys would need a table per pair of agents and could
// still produce a key the caller's schema does not have. Renaming only the name needs no table: the
// model reads the caller's own keys, and the call it gets back validates.
//
// The capability names below are what each tool documents. Sources are in
// docs/tool-vocabulary.md and repeated where a name comes from a specific page.

const (
	capShell   = "shell"
	capRead    = "read"
	capWrite   = "write"
	capEdit    = "edit"
	capPatch   = "patch"
	capSearch  = "search"
	capGlob    = "glob"
	capList    = "list"
	capPlan    = "plan"
	capWeb     = "web"
	capWebFind = "websearch"
	capMonitor = "monitor"
	capLSP     = "lsp"
	capSkill   = "skill"
	capAgent   = "agent"
	capAsk     = "ask"
)

// nameCap is every tool name intact knows, lowercased, and what it is for.
//
// Claude Code — code.claude.com/docs/en/tools.md, which states these "are the exact strings you use
// in permission rules, subagent tool lists, and hook matchers".
// Codex CLI — developers.openai.com codex prompting guide, and openai/codex codex-rs.
// Gemini CLI — google-gemini/gemini-cli docs/reference/tools.md.
// OpenCode — packages/opencode/src/tool on dev, where the shell tool is registered as `shell`.
var nameCap = map[string]string{
	// Claude Code
	"bash": capShell, "powershell": capShell, "monitor": capMonitor,
	"read": capRead, "write": capWrite, "edit": capEdit, "multiedit": capEdit,
	"notebookedit": capEdit, "notebookread": capRead,
	"grep": capSearch, "glob": capGlob,
	"todowrite": capPlan, "taskcreate": capPlan, "taskget": capPlan, "taskupdate": capPlan,
	"taskstop": capPlan, "tasklist": capList, "taskoutput": capRead,
	"webfetch": capWeb, "websearch": capWebFind,
	"lsp": capLSP, "skill": capSkill, "agent": capAgent, "askuserquestion": capAsk,
	"toolsearch": capList, "listmcpresourcestool": capList, "readmcpresourcetool": capRead,

	// Codex CLI
	"shell": capShell, "cmd": capShell, "run_terminal_cmd": capShell,
	"apply_patch": capPatch, "update_plan": capPlan, "todo_write": capPlan,
	"read_file": capRead, "list_dir": capList, "glob_file_search": capGlob, "rg": capSearch,

	// Gemini CLI
	"run_shell_command": capShell, "grep_search": capSearch, "search_file_content": capSearch,
	"list_directory": capList, "read_many_files": capRead, "write_file": capWrite,
	"replace": capEdit, "google_web_search": capWebFind, "write_todos": capPlan,
	"activate_skill": capSkill, "ask_user": capAsk, "get_internal_docs": capRead,
}

// tokenCap covers a name no tool documents. Only a token one vocabulary uses for one thing is listed,
// so a name built from these words reads as one capability rather than as a guess, and a name with no
// capability token is never rewritten.
var tokenCap = map[string]string{
	"shell": capShell, "bash": capShell, "sh": capShell, "zsh": capShell, "cmd": capShell,
	"command": capShell, "commands": capShell, "terminal": capShell, "console": capShell,
	"exec": capShell, "execute": capShell, "run": capShell, "spawn": capShell, "process": capShell,
	"monitor": capMonitor,
	"read":    capRead, "cat": capRead, "view": capRead, "open": capRead,
	"write": capWrite, "create": capWrite, "dump": capWrite, "save": capWrite,
	"edit": capEdit, "replace": capEdit, "modify": capEdit, "multiedit": capEdit,
	"patch": capPatch, "applypatch": capPatch,
	"grep": capSearch, "rg": capSearch, "ripgrep": capSearch, "ugrep": capSearch,
	"glob": capGlob, "filesearch": capGlob,
	"list": capList, "ls": capList, "dir": capList, "directory": capList,
	"plan": capPlan, "todo": capPlan, "todos": capPlan,
	"fetch": capWeb, "url": capWeb, "http": capWeb, "download": capWeb,
	"lsp": capLSP, "skill": capSkill,
	"agent": capAgent, "subagent": capAgent,
	"question": capAsk, "ask": capAsk,
}

// providerVocab is the name each provider's own agent uses for a capability. Sent under this name, a
// model answers under it too, which is the point: the reply needs no guessing.
//
// A capability with no entry is not offered under another name. Codex has no separate write tool, so a
// caller's Write stays Write there rather than becoming a patch.
//
// capEdit has no Codex entry on purpose, and this is the one mapping that stays a decision rather than
// an omission. A caller's Edit speaks old_string/new_string against a file it has read; Codex's
// apply_patch speaks a unified diff envelope (*** Begin Patch, hunks, add/update/delete/move). Renaming
// one to the other would not translate a tool, it would translate a payload, and the translation
// cannot be done here: producing a valid old_string needs the file, and intact never reads one. What
// it could produce is a call that looks right and fails, or applies to the wrong place, which is worse
// than the name the model got wrong.
//
// If this is ever wanted, the order to try is in docs/tool-vocabulary.md. The first option is the
// provider's own edit tool rather than a translation: OpenAI serves apply_patch as a server-defined
// Responses tool (tools=[{"type":"apply_patch"}]), Anthropic publishes text_editor with its own schema,
// and Gemini's is replace. Passing one of those through is a new capability, not a rename, and it
// carries the real semantics instead of a translation of them.
type providerVocab map[string]string

var codexVocab = providerVocab{
	capShell: "shell", capRead: "read_file", capList: "list_dir",
	capGlob: "glob_file_search", capSearch: "rg", capPlan: "update_plan", capPatch: "apply_patch",
}

var geminiVocab = providerVocab{
	capShell: "run_shell_command", capRead: "read_file", capWrite: "write_file",
	capEdit: "replace", capSearch: "grep_search", capGlob: "glob", capList: "list_directory",
	capPlan: "write_todos", capWeb: "web_fetch", capWebFind: "google_web_search", capAsk: "ask_user",
}

// vocabFor names the provider a request is going to. An account whose models take the names the caller
// sent, such as a Claude or a plain OpenAI Chat upstream, gets none: there is nothing to prefer, and
// renaming would only move a name the model already knows.
func vocabFor(provider string) providerVocab {
	switch provider {
	case "codex":
		return codexVocab
	case "antigravity", "gemini", "google":
		return geminiVocab
	}
	return nil
}

// capsOf is what a name is for: the documented one when a tool documents this exact name, else the
// capabilities its tokens carry.
func capsOf(name string) []string {
	if c, ok := nameCap[strings.ToLower(name)]; ok {
		return []string{c}
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range tokenizeToolName(name) {
		c, ok := tokenCap[strings.ToLower(tok)]
		if !ok || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// tokenizeToolName splits a name the way the model that wrote it saw it: TodoWrite is one word to
// whoever coined it, str_replace_editor is three, filePath is two.
func tokenizeToolName(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	var prev rune
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z' && i > 0 && (prev < 'A' || prev > 'Z') && !isDigit(prev):
			flush()
			cur = append(cur, r)
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return out
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// namespaceOf reads the server a name belongs to: mcp__github__create_issue is the github server's
// tool. A name with a namespace never leaves its namespace, in either direction. Renaming
// mcp__github__create_issue into a local Write produces a call the caller cannot detect as wrong: it
// writes a file instead of opening an issue, and it succeeds.
func namespaceOf(name string) string {
	parts := strings.Split(name, "__")
	if len(parts) < 3 {
		return ""
	}
	return strings.ToLower(strings.Join(parts[:len(parts)-1], "_"))
}

// ToolNames translates one request's tool names in both directions. A nil or zero value changes
// nothing, so a request with no tools needs no check.
type ToolNames struct {
	// toProvider is caller name -> provider name.
	toProvider map[string]string
	// toClient is provider name -> caller name.
	toClient map[string]string
	// caps is caller name -> capability, for the case where the model ignores the list it was given.
	caps map[string]string
	// declared counts the caller's tools a name may be chosen for. One tool has no second answer to
	// pick between.
	declared int
}

// NewToolNames builds the translation for one request. clientTools are the names the caller declared,
// provider is where the request is going.
//
// Only client-executed tools are offered: the caller sends these as function tools with a schema, and
// a hosted tool of its own is not among them.
//
// The caller's tools are counted for every provider, because a model reaching for its own vocabulary
// is a property of the model and not of the provider it was trained for: a Zen or Qwen model answers
// `shell` whichever OpenAI-compatible endpoint serves it. Counting is what lets ToClient match that
// answer back. A provider with no vocabulary of its own only skips the other half, the choice of which
// name to send, which is the part that is genuinely provider-specific.
func NewToolNames(clientTools []string, provider string) *ToolNames {
	t := &ToolNames{
		toProvider: map[string]string{},
		toClient:   map[string]string{},
		caps:       map[string]string{},
	}
	// A capability with several declared tools is never translated: which one the model meant is not
	// knowable, and picking one runs the wrong tool.
	byCap := map[string][]string{}
	for _, name := range clientTools {
		if name == "" || namespaceOf(name) != "" {
			continue
		}
		cs := capsOf(name)
		if len(cs) == 0 {
			continue
		}
		t.declared++
		t.caps[name] = cs[0]
		byCap[cs[0]] = append(byCap[cs[0]], name)
	}
	vocab := vocabFor(provider)
	if len(vocab) == 0 {
		return t
	}
	for capName, names := range byCap {
		if len(names) != 1 {
			continue
		}
		target, ok := vocab[capName]
		if !ok || target == names[0] {
			continue
		}
		t.toProvider[names[0]] = target
		t.toClient[target] = names[0]
	}
	return t
}

// ToProvider is the name to declare upstream. A name the provider already uses is its own.
func (t *ToolNames) ToProvider(name string) string {
	if t == nil {
		return name
	}
	if v, ok := t.toProvider[name]; ok {
		return v
	}
	return name
}

// ToClient is the caller's own name for a call the model made.
//
// A name intact sent maps straight back. A name intact did not send is matched on capability, and only
// when exactly one declared tool fits: the model reached for its own vocabulary (shell, read,
// str_replace_editor) and one of the caller's tools is unambiguously that. Two fitting tools means no
// change, because a caller with both Bash and PowerShell cannot run the one it did not get.
func (t *ToolNames) ToClient(name string) string {
	if t == nil || name == "" {
		return name
	}
	if v, ok := t.toClient[name]; ok {
		return v
	}
	if t.declared < 2 || namespaceOf(name) != "" {
		return name
	}
	want := capsOf(name)
	if len(want) == 0 {
		return name
	}
	var hit string
	for _, capName := range want {
		matches, only := 0, ""
		for _, declared := range t.declaredNames(capName) {
			if t.caps[declared] == capName {
				matches++
				only = declared
			}
		}
		// Every capability the name asks for must fit one declared tool, and it must be the same one:
		// a name asking for two things does not fit a tool that does one.
		if matches != 1 {
			return name
		}
		if hit != "" && hit != only {
			return name
		}
		hit = only
	}
	return hit
}

func (t *ToolNames) declaredNames(capName string) []string {
	out := make([]string, 0, len(t.caps))
	for name, c := range t.caps {
		if c == capName {
			out = append(out, name)
		}
	}
	return out
}
