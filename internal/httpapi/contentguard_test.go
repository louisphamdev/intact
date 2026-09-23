package httpapi

import (
	"path/filepath"
	"testing"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/store"
)

// The four tables below are the contract of the content guard. Every door into
// the filter table (the dashboard, MCP, the drift review, the error review)
// asks the same two functions, so one table holds for all of them at once.

// refusedFieldRules would leave a request the provider cannot answer.
var refusedFieldRules = []string{
	// the whole body, or a top-level field the request needs
	"*", "messages", "model", "input", "contents", "prompt", "request", "instructions", "tools", "system",
	// a container emptied of every item or key
	"request.*", "*.*", "messages.*", "tools.*", "contents.*",
	// the content itself, at any depth
	"request.contents", "*.contents", "request.contents.*.parts", "messages.*.content", "messages.*.role",
	"input.*.content", "contents.*.parts", "contents.*.parts.*.text", "request.systemInstruction",
	// a tool's identity
	"tools.*.name", "tools.*.function", "tools.*.function.name", "tools.*.input_schema",
}

// acceptedFieldRules remove something a provider refuses and nothing else.
var acceptedFieldRules = []string{
	"messages.*.cache_control", "request.generationConfig.foo", "reasoning_effort", "max_tokens",
	"messages.*.content.*.cache_control", "tools.*.input_schema.properties.*.encrypted",
	"anti_cheat", "context_management", "thinking", "tools.*.function.strict",
}

// refusedSystemRules match an ordinary system line, so they remove the prompt.
var refusedSystemRules = []string{`^.{2,}$`, `^.*$`, `^.{30,}$`, `[\s\S]{2,}`, `.*`, `^`, `.`, `(`}

// acceptedSystemRules match one marker line and nothing else.
var acceptedSystemRules = []string{`^x-anthropic-billing-header:.*$`, `^\s*<anti-cheat>.*$`}

func TestContentGuardHoldsEveryTableAtOnce(t *testing.T) {
	for _, p := range refusedFieldRules {
		if !essentialField(p) {
			t.Errorf("essentialField(%q) = false, want the rule refused", p)
		}
	}
	for _, p := range acceptedFieldRules {
		if essentialField(p) {
			t.Errorf("essentialField(%q) = true, want the rule accepted", p)
		}
	}
	for _, re := range refusedSystemRules {
		if !catchAllSystem(re) {
			t.Errorf("catchAllSystem(%q) = false, want the expression refused", re)
		}
	}
	for _, re := range acceptedSystemRules {
		if catchAllSystem(re) {
			t.Errorf("catchAllSystem(%q) = true, want the expression accepted", re)
		}
	}
}

// A rule stored before the guard existed, or written straight into the
// database, must not reach a request either.
func TestContentGuardDropsAStoredRuleOnReload(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.SaveFilter(store.Filter{Provider: "groq", Kind: filter.Field, Pattern: "messages.*.content", Enabled: true})
	s.SaveFilter(store.Filter{Provider: "groq", Kind: filter.System, Pattern: `^.{2,}$`, Enabled: true})
	s.SaveFilter(store.Filter{Provider: "groq", Kind: filter.Field, Pattern: "anti_cheat", Enabled: true})
	a, _ := newServer(s, nil, nil)
	a.reloadFilters()
	var got []string
	for _, r := range a.rulesFor("groq") {
		got = append(got, r.Kind+":"+r.Pattern)
	}
	for _, unsafe := range []string{"field:messages.*.content", `system:^.{2,}$`} {
		if contains(got, unsafe) {
			t.Errorf("rules = %v, want %s skipped", got, unsafe)
		}
	}
	if !contains(got, "field:anti_cheat") {
		t.Errorf("rules = %v, want the safe rule kept", got)
	}
}
