package servertools

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type obj = map[string]any

func decode(b []byte) (obj, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m obj
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}

func asObj(v any) obj   { m, _ := v.(map[string]any); return m }
func list(v any) []any  { l, _ := v.([]any); return l }
func str(v any) string  { s, _ := v.(string); return s }
func isTrue(v any) bool { b, _ := v.(bool); return b }
func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// number reads a JSON number of either decoded kind.
func number(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// textOf reads message or tool result content given as a string or as blocks,
// keeping the text of each block.
func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, p := range list(v) {
		if pm := asObj(p); str(pm["type"]) == "text" {
			parts = append(parts, str(pm["text"]))
		}
	}
	return strings.Join(parts, "\n")
}
