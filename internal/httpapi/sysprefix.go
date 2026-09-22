package httpapi

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
)

// withSystemPrefix makes an Anthropic request's system prompt start with the
// provider's required first block, splicing only the "system" value: a
// request with no system gets [prefix]; a string system s becomes [prefix, s];
// an array gets prefix prepended. A system that already starts with the
// prefix is left as it is.
func withSystemPrefix(p provider.Provider, body []byte) []byte {
	if p.SystemPrefix == "" {
		return body
	}
	if _, _, err := topLevelValue(body, "messages"); err != nil {
		return body // not a Messages request
	}
	text, _ := json.Marshal(p.SystemPrefix)
	block := []byte(`{"type":"text","text":` + string(text) + `}`)
	start, end, err := topLevelValue(body, "system")
	if err != nil {
		// No system: insert one right after the opening brace.
		i := bytes.IndexByte(body, '{')
		if i < 0 {
			return body
		}
		out := append([]byte{}, body[:i+1]...)
		out = append(out, `"system":[`...)
		out = append(out, block...)
		out = append(out, "],"...)
		return append(out, body[i+1:]...)
	}
	val := bytes.TrimSpace(body[start:end])
	var repl []byte
	switch {
	case len(val) > 0 && val[0] == '"':
		var s string
		if json.Unmarshal(val, &s) != nil {
			return body
		}
		if strings.HasPrefix(s, p.SystemPrefix) {
			return body
		}
		st, _ := json.Marshal(s)
		rest := []byte(`{"type":"text","text":` + string(st) + `}`)
		repl = append(append(append(append([]byte("["), block...), ','), rest...), ']')
		if s == "" {
			repl = append(append([]byte("["), block...), ']')
		}
	case len(val) > 0 && val[0] == '[':
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(val, &blocks) != nil {
			return body
		}
		if len(blocks) > 0 && strings.HasPrefix(blocks[0].Text, p.SystemPrefix) {
			return body
		}
		inner := bytes.TrimSpace(val[1 : len(val)-1])
		repl = append(append([]byte("["), block...), ']')
		if len(inner) > 0 {
			repl = append(append(append(append([]byte("["), block...), ','), inner...), ']')
		}
	case bytes.Equal(val, []byte("null")):
		repl = append(append([]byte("["), block...), ']')
	default:
		return body
	}
	out := append([]byte{}, body[:start]...)
	out = append(out, repl...)
	return append(out, body[end:]...)
}
