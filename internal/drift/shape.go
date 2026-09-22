// Package drift watches the structure of the JSON that passes through intact:
// what clients send and what providers answer. It learns the field paths of
// each endpoint and records a change when a field appears, disappears, or
// changes type, so an operator (or an agent over MCP) can decide whether a new
// field should be blacklisted before a provider starts refusing it, or notice
// that a provider changed its answer.
package drift

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Limits keep one huge document from costing more than it tells.
const (
	maxDepth = 12
	maxPaths = 1500
)

// dynamicParents hold keys that are names chosen by a client or a provider
// (tool parameters, question names, message ids), not fields of the API.
var dynamicParents = map[string]bool{
	"properties": true, "patternProperties": true, "$defs": true, "definitions": true,
	"answers": true, "criteria": true, "legend": true, "probabilities": true,
	"metadata": true, "client_metadata": true, "headers": true,
	"args": true, "arguments": true, "extra_body": true,
}

var idLike = regexp.MustCompile(`^([a-z]{2,6}_[A-Za-z0-9]{8,}|[0-9a-fA-F-]{16,}|.*\d.*\d.*\d.*)$`)

// Paths returns the field paths of a JSON document and the JSON type found at
// each: "messages[].content[].type" → "string". Object keys that are names
// rather than fields collapse to "{*}".
func Paths(doc []byte) map[string]string {
	d := json.NewDecoder(bytes.NewReader(doc))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil
	}
	out := map[string]string{}
	walk(v, "", "", 0, out)
	return out
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "unknown"
}

func walk(v any, path, parentKey string, depth int, out map[string]string) {
	if len(out) >= maxPaths || depth > maxDepth {
		return
	}
	t := typeOf(v)
	if path != "" {
		// A field seen as null in one message and as a value in another is one
		// field; keep the non-null type.
		if prev, ok := out[path]; !ok || prev == "null" {
			out[path] = t
		} else if t != "null" {
			out[path] = MergeTypes(prev, t)
		}
	}
	switch n := v.(type) {
	case map[string]any:
		// A very wide object is a map of names, not a record of fields (a
		// Responses object has about 40 fields, so the bar sits well above).
		dynamic := dynamicParents[parentKey] || len(n) > 96
		for k, c := range n {
			seg := k
			if dynamic || idLike.MatchString(k) {
				seg = "{*}"
			}
			walk(c, join(path, seg), k, depth+1, out)
		}
	case []any:
		for _, c := range n {
			walk(c, path+"[]", parentKey, depth+1, out)
		}
	}
}

// MergeTypes joins two type sets ("string|number") in a stable order.
func MergeTypes(a, b string) string {
	set := map[string]bool{}
	for _, t := range strings.Split(a+"|"+b, "|") {
		if t != "" && t != "null" {
			set[t] = true
		}
	}
	if len(set) == 0 {
		return "null"
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// SubsetOf reports whether every type in a is in b.
func SubsetOf(a, b string) bool {
	have := map[string]bool{}
	for _, t := range strings.Split(b, "|") {
		have[t] = true
	}
	for _, t := range strings.Split(a, "|") {
		if t != "null" && !have[t] {
			return false
		}
	}
	return true
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

// Event is one JSON document of a response, with the event it belongs to
// ("" for a whole JSON body).
type Event struct {
	Name string
	Body []byte
}

// SplitSSE returns the JSON documents of a server-sent-events stream, each
// named by its "type" field or its event: line. A document that is not JSON
// (such as [DONE]) is skipped.
func SplitSSE(stream []byte) []Event {
	var out []Event
	var name string
	var data [][]byte
	flush := func() {
		if len(data) > 0 {
			b := bytes.Join(data, []byte("\n"))
			if len(b) > 0 && b[0] == '{' {
				n := name
				var t struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(b, &t) == nil && t.Type != "" {
					n = t.Type
				}
				out = append(out, Event{Name: n, Body: b})
			}
		}
		name, data = "", nil
	}
	for _, line := range bytes.Split(stream, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("event:")):
			name = strings.TrimSpace(string(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
	flush()
	return out
}
