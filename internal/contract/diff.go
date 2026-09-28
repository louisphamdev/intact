package contract

import (
	"strconv"
	"strings"
)

// Candidate represents an unmatched input leaf from a diff.
type Candidate struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Len          int    `json:"len,omitempty"`
	Hash         string `json:"hash,omitempty"`
	Enum         string `json:"enum,omitempty"`
	Model        string `json:"model"`
	ClientFormat string `json:"clientFormat"`
	Direction    string `json:"direction"`
}

// Mapping maps an input path to an output path.
type Mapping struct {
	InPath  string `json:"inPath"`
	OutPath string `json:"outPath"`
}

// StaticNoiseList holds static field names that represent transport noise.
var StaticNoiseList = []string{
	"id",
	"created",
	"system_fingerprint",
	"date",
	"x-request-id",
}

// Diff compares input and output ShapeRecords with one-to-one matching.
// Request direction: input toolRequest, output request intact received.
// Response direction: input upstream answer, output toolResponse.
// Matching is one-to-one: an output leaf can match at most one input leaf. Each rule runs over
// every input leaf before the next, from the strictest: same path, approved mapping, same hash
// (len >= 8), same path under one renamed segment, then same enum anywhere. An enum anywhere
// comes last so that it cannot take the counterpart of a field a stricter rule would match.
// An unmatched input leaf is a candidate (at most 200), unless it is an empty container or the
// output was cut at that field, since its absence then proves nothing.
func Diff(in, out ShapeRecord, mappings []Mapping, noise []string, model, clientFormat, direction string) ([]Candidate, []string) {
	var outLeaves []Leaf
	var cutParents []string
	for _, rec := range out.Records {
		for _, leaf := range rec.Leaves {
			if leaf.Type == "cut" {
				cutParents = append(cutParents, strings.TrimSuffix(strings.TrimSuffix(leaf.Path, "{cut}"), "."))
				continue
			}
			outLeaves = append(outLeaves, leaf)
		}
	}
	outMatched := make([]bool, len(outLeaves))

	mappingMap := make(map[string]string)
	for _, m := range mappings {
		mappingMap[m.InPath] = m.OutPath
	}
	noiseMap := make(map[string]bool)
	for _, n := range StaticNoiseList {
		noiseMap[strings.ToLower(n)] = true
	}
	for _, n := range noise {
		noiseMap[strings.ToLower(n)] = true
	}

	var inLeaves []Leaf
	for _, rec := range in.Records {
		for _, inLeaf := range rec.Leaves {
			if inLeaf.Type == "cut" || inLeaf.Type == "invalid" {
				continue
			}
			segs := strings.Split(inLeaf.Path, ".")
			lastSeg := strings.TrimSuffix(segs[len(segs)-1], "[]")
			if noiseMap[strings.ToLower(lastSeg)] {
				continue
			}
			inLeaves = append(inLeaves, inLeaf)
		}
	}
	inMatched := make([]bool, len(inLeaves))

	// sameValue: a null output never stands for a value, and an enum must match its enum.
	sameValue := func(a, b Leaf) bool {
		if b.Type == "null" && a.Type != "null" {
			return false
		}
		return (a.Enum == "" && b.Enum == "") || a.Enum == b.Enum
	}
	pass := func(match func(a, b Leaf) bool) {
		for i, a := range inLeaves {
			if inMatched[i] {
				continue
			}
			for j, b := range outLeaves {
				if !outMatched[j] && match(a, b) {
					inMatched[i], outMatched[j] = true, true
					break
				}
			}
		}
	}
	pass(func(a, b Leaf) bool { return b.Path == a.Path && sameValue(a, b) })
	pass(func(a, b Leaf) bool {
		m, ok := mappingMap[a.Path]
		return ok && b.Path == m && sameValue(a, b)
	})
	pass(func(a, b Leaf) bool { return a.Len >= 8 && a.Hash != "" && b.Len >= 8 && b.Hash == a.Hash })
	pass(func(a, b Leaf) bool { return a.Type == b.Type && sameValue(a, b) && renamedSegment(a.Path, b.Path) })
	pass(func(a, b Leaf) bool { return a.Enum != "" && b.Enum == a.Enum })

	var candidates []Candidate
	for i, inLeaf := range inLeaves {
		if inMatched[i] || inLeaf.Type == "array" || inLeaf.Type == "object" || cutAt(inLeaf.Path, cutParents) {
			continue
		}
		if len(candidates) < 200 {
			candidates = append(candidates, Candidate{
				Path:         inLeaf.Path,
				Type:         inLeaf.Type,
				Len:          inLeaf.Len,
				Hash:         inLeaf.Hash,
				Enum:         inLeaf.Enum,
				Model:        model,
				ClientFormat: clientFormat,
				Direction:    direction,
			})
		}
	}

	// Collect up to 20 unmatched output paths for renamed options
	var unmatchedOutput []string
	for j, ol := range outLeaves {
		if !outMatched[j] && ol.Path != "" && ol.Type != "invalid" {
			unmatchedOutput = append(unmatchedOutput, ol.Path)
			if len(unmatchedOutput) >= 20 {
				break
			}
		}
	}

	return candidates, unmatchedOutput
}

// renamedSegment reports whether two paths name the same field under one renamed segment:
// the same start and the same end, one input segment in between, and one or two output
// segments in its place ("tools[].input_schema.x" and "tools[].function.parameters.x").
// A bare top-level rename ("thinking.budget", "reasoning.budget") shares too little to count.
func renamedSegment(in, out string) bool {
	a, b := strings.Split(in, "."), strings.Split(out, ".")
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	midIn, midOut := len(a)-p-s, len(b)-p-s
	return s >= 1 && (p >= 1 || s >= 2) && midIn == 1 && midOut >= 1 && midOut <= 2
}

// cutAt reports whether the output was cut at this input field or at one of its parents,
// under the same path or one renamed segment.
func cutAt(path string, cutParents []string) bool {
	segs := strings.Split(path, ".")
	for _, c := range cutParents {
		for k := 1; k <= len(segs); k++ {
			prefix := strings.Join(segs[:k], ".")
			if prefix == c || renamedSegment(prefix, c) {
				return true
			}
		}
	}
	return false
}

// CompareVersions compares two switcherVersions numerically by segment, then by UTC timestamp.
// e.g. "2.10.0+20260923T120000Z" vs "2.9.0+20260923T120000Z".
func CompareVersions(v1, v2 string) int {
	if v1 == v2 {
		return 0
	}
	p1 := strings.SplitN(v1, "+", 2)
	p2 := strings.SplitN(v2, "+", 2)

	segs1 := strings.Split(p1[0], ".")
	segs2 := strings.Split(p2[0], ".")

	maxLen := len(segs1)
	if len(segs2) > maxLen {
		maxLen = len(segs2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(segs1) {
			n1, _ = strconv.Atoi(segs1[i])
		}
		if i < len(segs2) {
			n2, _ = strconv.Atoi(segs2[i])
		}
		if n1 != n2 {
			if n1 > n2 {
				return 1
			}
			return -1
		}
	}

	var t1, t2 string
	if len(p1) > 1 {
		t1 = p1[1]
	}
	if len(p2) > 1 {
		t2 = p2[1]
	}
	if t1 < t2 {
		return -1
	} else if t1 > t2 {
		return 1
	}
	return 0
}
