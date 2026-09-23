package httpapi

import (
	"regexp"
	"strings"

	"github.com/louisphamdev/intact/internal/filter"
)

// One guard decides what a filter rule may never remove. The dashboard, MCP,
// the drift review and the error review all ask it, so a rule that empties
// every request cannot enter through a second door. The reviews act on a
// model's word, and that model reads a client's own request text.

// essentialLeaf are the last names of a path that carry the request itself, at
// any depth: messages, messages.*.content, contents.*.parts.*.text. A deeper
// path below one of them (messages.*.content.*.cache_control) is fine.
var essentialLeaf = map[string]bool{
	"content": true, "contents": true, "parts": true, "text": true, "role": true,
	"messages": true, "input": true, "prompt": true, "instructions": true, "system": true,
	"systemInstruction": true, "generationConfig": true, "model": true, "tools": true,
	"request": true, "stream": true,
}

// essentialPaths name what a request needs under a container, where the leaf
// alone says nothing: a tool without its name or its schema cannot be called.
var essentialPaths = [][]string{
	{"tools", "*", "name"},
	{"tools", "*", "function"},
	{"tools", "*", "function", "name"},
	{"tools", "*", "function", "parameters"},
	{"tools", "*", "input_schema"},
	{"tools", "*", "parameters"},
}

// systemLines are ordinary system-prompt lines. An expression that matches one
// of them removes the prompt instead of one marker line, so it is refused.
var systemLines = []string{
	"",
	"x",
	"You are a helpful assistant.",
	strings.Repeat("Answer in short sentences and keep the user's format. ", 4)[:200],
}

// essentialField reports a field rule that leaves a request the provider
// cannot answer: every item of a container, a content-bearing name, or a
// tool's identity.
func essentialField(pattern string) bool {
	segs := strings.Split(strings.TrimSpace(pattern), ".")
	if last := segs[len(segs)-1]; last == "*" || essentialLeaf[last] {
		return true
	}
	if len(segs) > 1 && segs[0] == "request" {
		segs = segs[1:] // Antigravity wraps the Gemini request under "request"
	}
	for _, e := range essentialPaths {
		if coversPath(segs, e) {
			return true
		}
	}
	return false
}

// coversPath reports whether p is e or an ancestor of it. "*" on either side
// matches one segment.
func coversPath(p, e []string) bool {
	if len(p) > len(e) {
		return false
	}
	for i, s := range p {
		if s != e[i] && s != "*" && e[i] != "*" {
			return false
		}
	}
	return true
}

// catchAllSystem reports a system expression that removes the prompt: it does
// not compile, or it matches an ordinary line.
func catchAllSystem(pattern string) bool {
	re, err := regexp.Compile("(?m)" + strings.TrimSpace(pattern))
	if err != nil {
		return true
	}
	for _, line := range systemLines {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// unsafeFilter says why a rule may not be installed, or "" when it is safe.
func unsafeFilter(kind, pattern string) string {
	switch kind {
	case filter.Field:
		if essentialField(pattern) {
			return "this pattern would strip an essential field from every request; narrow it"
		}
	case filter.System:
		if catchAllSystem(pattern) {
			return "this expression matches ordinary system lines; it would remove the whole system prompt"
		}
	}
	return ""
}
