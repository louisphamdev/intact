package websearch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Hosted reports whether a Messages request declares Anthropic's hosted web
// search tool: a tool typed web_search_..., or an untyped one called web_search
// or WebSearch that carries no input schema (a client tool of that name has one,
// and runs on the client).
func Hosted(body []byte) bool {
	m, err := decode(body)
	if err != nil {
		return false
	}
	for _, t := range list(m["tools"]) {
		if isHosted(t) {
			return true
		}
	}
	return false
}

// IsHostedTool reports whether one entry of a request's tools is Anthropic's
// hosted web search tool.
func IsHostedTool(t any) bool { return isHosted(t) }

func isHosted(t any) bool {
	tool, _ := t.(map[string]any)
	typ, _ := tool["type"].(string)
	if strings.HasPrefix(typ, "web_search") {
		return true
	}
	name, _ := tool["name"].(string)
	return (typ == "" || typ == "custom") && tool["input_schema"] == nil && (name == "web_search" || name == "WebSearch")
}

// queryPrefix is how Claude Code words the request its WebSearch tool sends.
const queryPrefix = "perform a web search for the query:"

// Query reads what to search for from the last user message. Claude Code writes
// "Perform a web search for the query: <query>"; any other message is searched
// as it stands.
func Query(body []byte) string {
	m, err := decode(body)
	if err != nil {
		return ""
	}
	msgs := list(m["messages"])
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, _ := msgs[i].(map[string]any)
		if msg["role"] != "user" {
			continue
		}
		q := strings.TrimSpace(text(msg["content"]))
		if strings.HasPrefix(strings.ToLower(q), queryPrefix) {
			q = strings.TrimSpace(q[len(queryPrefix):])
		}
		if r := []rune(q); len(r) > 400 {
			q = string(r[:400])
		}
		return q
	}
	return ""
}

// Rewrite turns a request for a hosted search into one any model can answer: the
// hosted tool (and a tool_choice that names it) goes, and the results, or the
// reason there are none, are added to the system prompt. failure is empty when the
// search ran.
func Rewrite(body []byte, query string, results []Result, failure string) ([]byte, error) {
	m, err := decode(body)
	if err != nil {
		return nil, err
	}
	var kept []any
	for _, t := range list(m["tools"]) {
		if !isHosted(t) {
			kept = append(kept, t)
		}
	}
	if len(kept) > 0 {
		m["tools"] = kept
	} else {
		delete(m, "tools")
	}
	if tc, ok := m["tool_choice"].(map[string]any); ok {
		if name, _ := tc["name"].(string); name == "web_search" || name == "WebSearch" || len(kept) == 0 {
			delete(m, "tool_choice")
		}
	}
	ev := evidence(query, results, failure)
	switch s := m["system"].(type) {
	case string:
		m["system"] = s + "\n\n" + ev
	case []any:
		m["system"] = append(s, map[string]any{"type": "text", "text": ev})
	default:
		m["system"] = ev
	}
	return json.Marshal(m)
}

func evidence(query string, results []Result, failure string) string {
	var b strings.Builder
	switch {
	case failure != "":
		fmt.Fprintf(&b, "The web search for %q could not be run (%s). Tell the user the search failed; do not invent results or sources.", query, failure)
	case len(results) == 0:
		fmt.Fprintf(&b, "The web search for %q returned no results. Say so; do not invent results or sources.", query)
	default:
		fmt.Fprintf(&b, "Web search results for %q. They are the only sources you may cite; name each by its URL.\n", query)
		for i, r := range results {
			fmt.Fprintf(&b, "\n[%d] %s\nURL: %s\n", i+1, r.Title, r.URL)
			if r.Age != "" {
				fmt.Fprintf(&b, "Age: %s\n", r.Age)
			}
			if r.Snippet != "" {
				fmt.Fprintf(&b, "%s\n", r.Snippet)
			}
		}
		b.WriteString("\nAnswer from these results. Do not claim to have looked at anything beyond them.")
	}
	return b.String()
}

func decode(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}

func list(v any) []any { l, _ := v.([]any); return l }

// text reads message content given as a string or as text blocks.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, p := range list(v) {
		if pm, ok := p.(map[string]any); ok {
			if s, ok := pm["text"].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}
