// Package filter removes what a provider refuses from a request before it is
// sent. Providers fail fast on a field they do not know, so a quick rule is the
// fix, without a rebuild.
//
// Kinds:
//
//   - field: a dot path into the JSON body; "*" matches any key or array item.
//     "thinking", "messages.*.cache_control", "tools.*.function.strict".
//   - schema: a key removed at every depth of each tool's JSON schema (OpenAI
//     tools[].function.parameters, Anthropic tools[].input_schema). "$id".
//   - system: a regular expression; matching lines are removed from the system
//     prompt (Anthropic "system", OpenAI system and developer messages).
//   - header: a request header that is not sent. "anthropic-beta".
package filter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Kinds a rule can have.
const (
	Field  = "field"
	Schema = "schema"
	System = "system"
	Header = "header"
)

// Rule is one compiled filter.
type Rule struct {
	Kind    string
	Pattern string
	path    []string
	re      *regexp.Regexp
}

// Compile checks and prepares a rule.
func Compile(kind, pattern string) (Rule, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return Rule{}, fmt.Errorf("empty pattern")
	}
	r := Rule{Kind: kind, Pattern: pattern}
	switch kind {
	case Field:
		r.path = strings.Split(pattern, ".")
		for _, p := range r.path {
			if p == "" {
				return Rule{}, fmt.Errorf("empty segment in %q", pattern)
			}
		}
	case Schema:
	case Header:
		r.Pattern = http.CanonicalHeaderKey(pattern)
	case System:
		re, err := regexp.Compile("(?m)" + pattern)
		if err != nil {
			return Rule{}, fmt.Errorf("bad expression: %w", err)
		}
		r.re = re
	default:
		return Rule{}, fmt.Errorf("unknown kind %q", kind)
	}
	return r, nil
}

// Headers returns the headers the rules drop.
func Headers(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		if r.Kind == Header {
			out = append(out, r.Pattern)
		}
	}
	return out
}

// Apply removes what the rules match from a JSON body. The body is re-encoded
// only when a rule removed something, so an untouched request keeps its bytes.
// A body that is not a JSON object is returned as it is.
func Apply(body []byte, rules []Rule) ([]byte, bool) {
	var bodyRules []Rule
	for _, r := range rules {
		if r.Kind != Header {
			bodyRules = append(bodyRules, r)
		}
	}
	if len(bodyRules) == 0 {
		return body, false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil || m == nil {
		return body, false
	}
	changed := false
	for _, r := range bodyRules {
		switch r.Kind {
		case Field:
			changed = dropPath(m, r.path) || changed
		case Schema:
			for _, s := range toolSchemas(m) {
				changed = dropKey(s, r.Pattern) || changed
			}
		case System:
			changed = cleanSystem(m, r.re) || changed
		}
	}
	if !changed {
		return body, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(m) != nil {
		return body, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// dropPath deletes the key at path, following "*" through every key or item.
func dropPath(v any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	seg, rest := path[0], path[1:]
	switch n := v.(type) {
	case map[string]any:
		if len(rest) == 0 {
			if seg == "*" {
				had := len(n) > 0
				clear(n)
				return had
			}
			_, had := n[seg]
			delete(n, seg)
			return had
		}
		if seg == "*" {
			changed := false
			for _, c := range n {
				changed = dropPath(c, rest) || changed
			}
			return changed
		}
		return dropPath(n[seg], rest)
	case []any:
		if seg != "*" || len(rest) == 0 {
			return false
		}
		changed := false
		for _, c := range n {
			changed = dropPath(c, rest) || changed
		}
		return changed
	}
	return false
}

// toolSchemas returns the JSON schema of each tool, in either shape.
func toolSchemas(m map[string]any) []any {
	var out []any
	tools, _ := m["tools"].([]any)
	for _, t := range tools {
		tm, _ := t.(map[string]any)
		if s, ok := tm["input_schema"]; ok {
			out = append(out, s)
		}
		if fn, ok := tm["function"].(map[string]any); ok {
			if s, ok := fn["parameters"]; ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// dropKey removes key at every depth. Under "properties" the keys are the names
// of the tool's own parameters, so a parameter that happens to be called like
// the key is kept and only its schema is cleaned.
func dropKey(v any, key string) bool {
	changed := false
	switch n := v.(type) {
	case map[string]any:
		if _, ok := n[key]; ok {
			delete(n, key)
			changed = true
		}
		for k, c := range n {
			if k == "properties" {
				if props, ok := c.(map[string]any); ok {
					for _, p := range props {
						changed = dropKey(p, key) || changed
					}
					continue
				}
			}
			changed = dropKey(c, key) || changed
		}
	case []any:
		for _, c := range n {
			changed = dropKey(c, key) || changed
		}
	}
	return changed
}

// cleanSystem removes the lines re matches from the system prompt.
func cleanSystem(m map[string]any, re *regexp.Regexp) bool {
	changed := false
	clean := func(s string) (string, bool) {
		out := re.ReplaceAllString(s, "")
		if out == s {
			return s, false
		}
		// Removing a whole line leaves its newline; drop the blank lines left.
		for strings.Contains(out, "\n\n\n") {
			out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
		}
		return strings.TrimLeft(out, "\n"), true
	}
	cleanContent := func(c any) any {
		switch v := c.(type) {
		case string:
			if s, ok := clean(v); ok {
				changed = true
				return s
			}
		case []any:
			for _, b := range v {
				if bm, ok := b.(map[string]any); ok {
					if t, ok := bm["text"].(string); ok {
						if s, ok := clean(t); ok {
							changed = true
							bm["text"] = s
						}
					}
				}
			}
		}
		return c
	}
	if s, ok := m["system"]; ok {
		m["system"] = cleanContent(s)
	}
	msgs, _ := m["messages"].([]any)
	for _, x := range msgs {
		if mm, ok := x.(map[string]any); ok && (mm["role"] == "system" || mm["role"] == "developer") {
			mm["content"] = cleanContent(mm["content"])
		}
	}
	return changed
}
