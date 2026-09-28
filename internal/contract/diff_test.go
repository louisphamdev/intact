package contract

import (
	"testing"
)

func TestDiff_MatchingOneToOne(t *testing.T) {
	// Spec section 14:
	// "Matching: two input fields share one value of len >= 8, the converter drops one of them,
	// and the diff gives exactly one candidate, for the dropped field."
	sharedHash := "aabbccddeeff" // HMAC truncated hex or test hash
	sharedLen := 12

	inRecord := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{
				Leaves: []Leaf{
					{Path: "field_kept", Type: "string", Len: sharedLen, Hash: sharedHash},
					{Path: "field_dropped", Type: "string", Len: sharedLen, Hash: sharedHash},
				},
			},
		},
	}

	outRecord := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{
				Leaves: []Leaf{
					// Output has only one field with this shared value (renamed/different path)
					{Path: "output_renamed", Type: "string", Len: sharedLen, Hash: sharedHash},
				},
			},
		},
	}

	candidates, _ := Diff(inRecord, outRecord, nil, nil, "test-model", "anthropic", "request")
	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 candidate for the dropped field, got %d", len(candidates))
	}
	if candidates[0].Path != "field_dropped" {
		t.Fatalf("expected candidate path to be 'field_dropped', got '%s'", candidates[0].Path)
	}
}

func TestCompareVersions(t *testing.T) {
	// Spec section 14: "2.10.0 compares greater than 2.9.0"
	if CompareVersions("2.10.0+20260923T120000Z", "2.9.0+20260923T120000Z") <= 0 {
		t.Fatal("2.10.0 should be greater than 2.9.0")
	}
	if CompareVersions("2.9.0+20260923T120000Z", "2.10.0+20260923T120000Z") >= 0 {
		t.Fatal("2.9.0 should be less than 2.10.0")
	}
	// Same version number, compare timestamp
	if CompareVersions("2.9.0+20260923T140000Z", "2.9.0+20260923T120000Z") <= 0 {
		t.Fatal("later timestamp should compare greater")
	}
	if CompareVersions("2.9.0+20260923T120000Z", "2.9.0+20260923T120000Z") != 0 {
		t.Fatal("identical versions should compare equal")
	}
}

func TestC9DiffEnumAndNullCandidates(t *testing.T) {
	inRec1 := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{Leaves: []Leaf{{Path: "status", Type: "string", Enum: "stop"}}},
		},
	}
	outRec1 := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{Leaves: []Leaf{{Path: "status", Type: "string"}}},
		},
	}
	cands1, _ := Diff(inRec1, outRec1, nil, nil, "m", "anthropic", "response")
	if len(cands1) != 1 || cands1[0].Path != "status" {
		t.Fatalf("case 1: expected 1 candidate for status, got %+v", cands1)
	}

	outRec2 := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{Leaves: []Leaf{{Path: "status", Type: "null"}}},
		},
	}
	cands2, _ := Diff(inRec1, outRec2, nil, nil, "m", "anthropic", "response")
	if len(cands2) != 1 || cands2[0].Path != "status" {
		t.Fatalf("case 2: expected 1 candidate for status, got %+v", cands2)
	}
}

func leaves(ls ...Leaf) ShapeRecord {
	return ShapeRecord{ReducerVersion: ReducerVersion, Records: []Record{{Leaves: ls}}}
}

func candidatePaths(c []Candidate) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.Path)
	}
	return out
}

// A converter that moves a whole schema (tools[].input_schema -> tools[].function.parameters)
// keeps every field under it: the diff matches them through the renamed segment, before a
// type enum anywhere else can take their counterpart.
func TestDiffMatchesFieldsUnderARenamedSegment(t *testing.T) {
	in := leaves(
		Leaf{Path: "tools[].input_schema.properties.{*}.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].input_schema.properties.{*}.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].input_schema.required[]", Type: "string", Len: 5},
		Leaf{Path: "output_config.format.schema.required[]", Type: "string", Len: 5},
		Leaf{Path: "thinking.budget", Type: "number"},
	)
	out := leaves(
		Leaf{Path: "response_format.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].function.parameters.properties.{*}.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].function.parameters.properties.{*}.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].function.parameters.required[]", Type: "string", Len: 5},
		Leaf{Path: "reasoning.budget", Type: "number"},
	)
	got := candidatePaths(func() []Candidate { c, _ := Diff(in, out, nil, nil, "m", "anthropic", "request"); return c }())
	// Only the real loss is left; a top-level key that shares a last segment is not a rename.
	if len(got) != 2 || got[0] != "output_config.format.schema.required[]" || got[1] != "thinking.budget" {
		t.Fatalf("candidates = %v", got)
	}
}

// The reducer cuts a path deeper than maxDepth. A converter that adds a segment pushes a field
// past the cut on the output side only: its absence there proves nothing.
func TestDiffIgnoresFieldsTheOutputWasCutAt(t *testing.T) {
	in := leaves(
		Leaf{Path: "tools[].input_schema.a.b.type", Type: "string", Enum: "string"},
		Leaf{Path: "tools[].input_schema.a.b.c.description", Type: "string", Len: 9},
		Leaf{Path: "tools[].input_schema.x.lost", Type: "string", Len: 9},
	)
	out := leaves(
		Leaf{Path: "tools[].function.parameters.a.b.type.{cut}", Type: "cut"},
		Leaf{Path: "tools[].function.parameters.a.b.c.{cut}", Type: "cut"},
	)
	got := candidatePaths(func() []Candidate { c, _ := Diff(in, out, nil, nil, "m", "anthropic", "request"); return c }())
	if len(got) != 1 || got[0] != "tools[].input_schema.x.lost" {
		t.Fatalf("candidates = %v", got)
	}
}

// An empty array or object carries no value, so dropping it loses nothing.
func TestDiffIgnoresEmptyContainers(t *testing.T) {
	in := leaves(Leaf{Path: "tools", Type: "array"}, Leaf{Path: "metadata", Type: "object"}, Leaf{Path: "stop", Type: "string", Len: 3})
	got := candidatePaths(func() []Candidate { c, _ := Diff(in, leaves(), nil, nil, "m", "anthropic", "request"); return c }())
	if len(got) != 1 || got[0] != "stop" {
		t.Fatalf("candidates = %v", got)
	}
}
