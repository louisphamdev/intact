package translate

import (
	"bytes"
	"encoding/json"
	"strings"
)

// blankStandIn replaces a message whose text was all blank: Anthropic refuses
// an empty message as it refuses an empty block.
const blankStandIn = "(empty)"

// WithoutBlankText drops the empty and whitespace-only text blocks of an
// Anthropic Messages body, which Anthropic answers with a 400 ("text content
// blocks must be non-empty"). A body with none goes out unchanged, byte for byte.
func WithoutBlankText(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m obj
	if dec.Decode(&m) != nil {
		return body
	}
	changed := false
	switch s := m["system"].(type) {
	case string:
		if strings.TrimSpace(s) == "" {
			delete(m, "system")
			changed = true
		}
	case []any:
		kept, dropped := withoutBlankBlocks(s)
		if dropped {
			changed = true
			if len(kept) == 0 {
				delete(m, "system")
			} else {
				m["system"] = kept
			}
		}
	}
	msgs := list(m["messages"])
	if n := len(msgs); n > 0 && str(asObj(msgs[n-1])["role"]) == "assistant" && isBlankContent(asObj(msgs[n-1])["content"]) {
		m["messages"] = msgs[:n-1]
		msgs = msgs[:n-1]
		changed = true
	}
	for _, raw := range msgs {
		msg := asObj(raw)
		switch c := msg["content"].(type) {
		case string:
			if strings.TrimSpace(c) == "" {
				msg["content"] = blankStandIn
				changed = true
			}
		case []any:
			kept, dropped := withoutBlankBlocks(c)
			if dropped {
				changed = true
				if len(kept) == 0 {
					kept = []any{obj{"type": "text", "text": blankStandIn}}
				}
				msg["content"] = kept
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func withoutBlankBlocks(blocks []any) ([]any, bool) {
	kept := blocks[:0:0]
	for _, b := range blocks {
		bm := asObj(b)
		if str(bm["type"]) == "text" && strings.TrimSpace(str(bm["text"])) == "" {
			continue
		}
		kept = append(kept, b)
	}
	return kept, len(kept) != len(blocks)
}

func isBlankContent(c any) bool {
	switch v := c.(type) {
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		kept, _ := withoutBlankBlocks(v)
		return len(kept) == 0
	}
	return false
}

// WithoutDuplicateReasoning drops `reasoning` from a chat message that also
// carries `reasoning_content`: serde-based upstreams read both as one field
// and refuse the pair. A body with no such message goes out unchanged.
func WithoutDuplicateReasoning(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"reasoning_content"`)) || !bytes.Contains(body, []byte(`"reasoning"`)) {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m obj
	if dec.Decode(&m) != nil {
		return body
	}
	changed := false
	for _, raw := range list(m["messages"]) {
		msg := asObj(raw)
		if _, both := msg["reasoning_content"]; both {
			if _, ok := msg["reasoning"]; ok {
				delete(msg, "reasoning")
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
