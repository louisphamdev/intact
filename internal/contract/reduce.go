// Package contract reduces requests and responses into shape records and diffs them.
package contract

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/louisphamdev/intact/internal/drift"
)

// ReducerVersion is the current reducer version.
const ReducerVersion = 1

const (
	maxDepth           = 12
	maxLeavesPerEvent  = 1500
	maxLeavesPerRecord = 5000
)

var (
	validSegment   = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)
	validEnumValue = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
)

var enumLeafNames = map[string]bool{
	"type":          true,
	"role":          true,
	"object":        true,
	"finish_reason": true,
	"stop_reason":   true,
	"status":        true,
	"event":         true,
	"model":         true,
}

var allowedStreamEvents = map[string]bool{
	"":                                       true,
	"message_start":                          true,
	"content_block_start":                    true,
	"content_block_delta":                    true,
	"content_block_stop":                     true,
	"message_delta":                          true,
	"message_stop":                           true,
	"ping":                                   true,
	"error":                                  true,
	"response.created":                       true,
	"response.done":                          true,
	"response.completed":                     true,
	"response.output_item.added":             true,
	"response.output_item.done":              true,
	"response.content_part.added":            true,
	"response.content_part.done":             true,
	"response.text.delta":                    true,
	"response.text.done":                     true,
	"response.output_text.delta":             true,
	"response.output_text.done":              true,
	"response.function_call_arguments.delta": true,
	"response.function_call_arguments.done":  true,
	"rate_limits.updated":                    true,
}

// Leaf is one field or terminal node of a shape record.
type Leaf struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Len    int    `json:"len,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Enum   string `json:"enum,omitempty"`
	Deltas int    `json:"deltas,omitempty"`
}

// Record holds leaves of one document or stream event.
type Record struct {
	Event  string `json:"event,omitempty"`
	Leaves []Leaf `json:"leaves"`
}

// ShapeRecord stores the reduced representation of an exchange half.
type ShapeRecord struct {
	ReducerVersion int      `json:"reducerVersion"`
	Records        []Record `json:"records"`
}

// MarshalJSON returns the JSON encoding of the shape record.
func (r ShapeRecord) MarshalJSON() ([]byte, error) {
	type Alias ShapeRecord
	return json.Marshal((Alias)(r))
}

// Reduce turns a body into a shape record with raw text dropped.
func Reduce(body []byte, sse bool, key []byte) ShapeRecord {
	out := ShapeRecord{
		ReducerVersion: ReducerVersion,
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		out.Records = []Record{{Leaves: []Leaf{{Path: "", Type: "invalid"}}}}
		return out
	}

	if !sse {
		leaves := reduceJSON(trimmed, key)
		if len(leaves) == 0 {
			leaves = []Leaf{{Path: "", Type: "invalid"}}
		}
		out.Records = []Record{{Leaves: leaves}}
		return out
	}

	events := drift.SplitSSE(body)
	if len(events) == 0 {
		out.Records = []Record{{Leaves: []Leaf{{Path: "", Type: "invalid"}}}}
		return out
	}

	type deltaAcc struct {
		text   strings.Builder
		count  int
		path   string
		pType  string
		evName string
	}
	deltas := map[string]*deltaAcc{}
	deltaNonDeltaLeaves := map[string][]Leaf{}
	deltaSeenPaths := map[string]map[string]bool{}
	deltaRecordIdx := map[string]int{}

	var records []Record
	totalLeaves := 0

	for _, ev := range events {
		evName := ev.Name
		if !allowedStreamEvents[evName] {
			evName = "{*}"
		}

		isDeltaEvent := strings.Contains(evName, "delta") || evName == ""
		evLeaves := reduceJSON(ev.Body, key)

		if isDeltaEvent {
			hasDeltaLeaf := false
			for _, lf := range evLeaves {
				if lf.Type == "string" && lf.Enum == "" && (strings.Contains(lf.Path, "delta") || strings.Contains(lf.Path, "content") || strings.Contains(lf.Path, "text") || strings.Contains(lf.Path, "arguments")) {
					hasDeltaLeaf = true
					acc, ok := deltas[lf.Path]
					if !ok {
						acc = &deltaAcc{path: lf.Path, pType: lf.Type, evName: evName}
						deltas[lf.Path] = acc
					}
					rawStr := extractStringByPath(ev.Body, lf.Path)
					acc.text.WriteString(rawStr)
					acc.count++
				}
			}
			if hasDeltaLeaf {
				if deltaSeenPaths[evName] == nil {
					deltaSeenPaths[evName] = make(map[string]bool)
					deltaRecordIdx[evName] = len(records)
					records = append(records, Record{Event: evName})
				}
				for _, lf := range evLeaves {
					isDelta := lf.Type == "string" && lf.Enum == "" && (strings.Contains(lf.Path, "delta") || strings.Contains(lf.Path, "content") || strings.Contains(lf.Path, "text") || strings.Contains(lf.Path, "arguments"))
					if !isDelta && !deltaSeenPaths[evName][lf.Path] {
						deltaSeenPaths[evName][lf.Path] = true
						deltaNonDeltaLeaves[evName] = append(deltaNonDeltaLeaves[evName], lf)
					}
				}
				continue
			}
		}

		if totalLeaves+len(evLeaves) > maxLeavesPerRecord {
			rem := maxLeavesPerRecord - totalLeaves
			if rem > 0 {
				evLeaves = evLeaves[:rem]
			} else {
				break
			}
		}
		totalLeaves += len(evLeaves)
		records = append(records, Record{
			Event:  evName,
			Leaves: evLeaves,
		})
	}

	if len(deltas) > 0 {
		byEvent := map[string][]Leaf{}
		for path, acc := range deltas {
			combined := acc.text.String()
			lf := Leaf{
				Path:   path,
				Type:   acc.pType,
				Len:    len(combined),
				Hash:   computeHash(key, combined),
				Deltas: acc.count,
			}
			byEvent[acc.evName] = append(byEvent[acc.evName], lf)
		}
		for evName, deltaLfs := range byEvent {
			combinedLfs := append(deltaNonDeltaLeaves[evName], deltaLfs...)
			if idx, ok := deltaRecordIdx[evName]; ok {
				records[idx].Leaves = combinedLfs
			} else {
				records = append(records, Record{
					Event:  evName,
					Leaves: combinedLfs,
				})
			}
		}
	}

	out.Records = records
	return out
}

func extractStringByPath(doc []byte, targetPath string) string {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return ""
	}
	var found string
	var search func(curr any, currPath string) bool
	search = func(curr any, currPath string) bool {
		if currPath == targetPath {
			if s, ok := curr.(string); ok {
				found = s
				return true
			}
		}
		switch n := curr.(type) {
		case map[string]any:
			for k, val := range n {
				p := k
				if currPath != "" {
					p = currPath + "." + k
				}
				if search(val, p) {
					return true
				}
			}
		case []any:
			for _, item := range n {
				p := currPath + "[]"
				if search(item, p) {
					return true
				}
			}
		}
		return false
	}
	search(v, "")
	return found
}

func computeHash(key []byte, val string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(val))
	sum := mac.Sum(nil)
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return hex.EncodeToString(sum)
}

func reduceJSON(doc []byte, key []byte) []Leaf {
	d := json.NewDecoder(bytes.NewReader(doc))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil
	}
	var leaves []Leaf
	walk(v, "", "", false, false, 0, key, &leaves)
	return leaves
}

func walk(v any, path, parentKey string, underJSON, underDataParent bool, depth int, key []byte, leaves *[]Leaf) {
	if len(*leaves) >= maxLeavesPerEvent {
		return
	}
	if depth > maxDepth {
		*leaves = append(*leaves, Leaf{
			Path: join(path, "{cut}"),
			Type: "cut",
		})
		return
	}

	switch n := v.(type) {
	case map[string]any:
		if len(n) == 0 {
			*leaves = append(*leaves, Leaf{
				Path: path,
				Type: "object",
			})
			return
		}
		wide := drift.WideObject(len(n))
		dataP := underDataParent || drift.DataParents[parentKey]
		dynP := drift.DynamicParents[parentKey]
		underAnthropicInput := strings.Contains(path, "content[].input")

		for k, c := range n {
			seg := k
			if wide || dynP || dataP || underJSON || underAnthropicInput || !validSegment.MatchString(k) || drift.IDLike(k) {
				seg = "{*}"
			}
			childPath := join(path, seg)
			walk(c, childPath, k, underJSON, dataP, depth+1, key, leaves)
		}
	case []any:
		if len(n) == 0 {
			*leaves = append(*leaves, Leaf{
				Path: path,
				Type: "array",
			})
			return
		}
		for _, c := range n {
			walk(c, path+"[]", parentKey, underJSON, underDataParent, depth+1, key, leaves)
		}
	case string:
		trimmed := strings.TrimSpace(n)
		if !underJSON && (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") ||
			strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			var parsed any
			d := json.NewDecoder(strings.NewReader(trimmed))
			d.UseNumber()
			if d.Decode(&parsed) == nil {
				walk(parsed, path+"#json", parentKey, true, underDataParent, depth+1, key, leaves)
				return
			}
		}

		lastSeg := parentKey
		if idx := strings.LastIndex(path, "."); idx >= 0 {
			lastSeg = path[idx+1:]
		}
		lastSeg = strings.TrimRight(lastSeg, "[]")

		underAnthropicInput := strings.Contains(path, "content[].input")
		isEnum := !underJSON && !underDataParent && !underAnthropicInput &&
			(enumLeafNames[lastSeg] || allowedStreamEvents[lastSeg]) &&
			validEnumValue.MatchString(n)

		if isEnum {
			*leaves = append(*leaves, Leaf{
				Path: path,
				Type: "string",
				Enum: n,
			})
		} else {
			*leaves = append(*leaves, Leaf{
				Path: path,
				Type: "string",
				Len:  len(n),
				Hash: computeHash(key, n),
			})
		}
	case json.Number:
		*leaves = append(*leaves, Leaf{
			Path: path,
			Type: "number",
		})
	case bool:
		*leaves = append(*leaves, Leaf{
			Path: path,
			Type: "boolean",
		})
	case nil:
		*leaves = append(*leaves, Leaf{
			Path: path,
			Type: "null",
		})
	}
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}
