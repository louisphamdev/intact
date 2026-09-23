package contract

import (
	"bytes"
	"strings"
	"testing"
)

var testKey = []byte("12345678901234567890123456789012")

func TestReduceBadJSON(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("not json"), []byte("{bad")} {
		rec := Reduce(body, false, testKey)
		if len(rec.Records) != 1 || len(rec.Records[0].Leaves) != 1 {
			t.Fatalf("expected 1 record with 1 leaf, got %+v", rec)
		}
		lf := rec.Records[0].Leaves[0]
		if lf.Type != "invalid" {
			t.Fatalf("expected invalid leaf, got %+v", lf)
		}
	}
}

func TestReduceEnumAndString(t *testing.T) {
	// An allowlisted enum keeps its value, any other string keeps only length and hash.
	body := []byte(`{"type":"message_start","custom":"secret value to hash"}`)
	rec := Reduce(body, false, testKey)
	if len(rec.Records) != 1 {
		t.Fatalf("expected 1 record, got %+v", rec)
	}
	leaves := map[string]Leaf{}
	for _, lf := range rec.Records[0].Leaves {
		leaves[lf.Path] = lf
	}
	tEnum, ok := leaves["type"]
	if !ok || tEnum.Enum != "message_start" || tEnum.Hash != "" {
		t.Fatalf("type leaf invalid: %+v", tEnum)
	}
	cStr, ok := leaves["custom"]
	if !ok || cStr.Enum != "" || cStr.Len != len("secret value to hash") || cStr.Hash == "" {
		t.Fatalf("custom leaf invalid: %+v", cStr)
	}
}

func TestReduceToolArgumentsString(t *testing.T) {
	// Tool arguments {"my secret file":1,"status":"abc"} give no segment my secret file and no enum abc.
	body := []byte(`{"arguments":"{\"my secret file\":1,\"status\":\"abc\"}"}`)
	rec := Reduce(body, false, testKey)
	raw, _ := rec.MarshalJSON()
	s := string(raw)
	if strings.Contains(s, "my secret file") {
		t.Fatalf("output leaked 'my secret file': %s", s)
	}
	if strings.Contains(s, `"enum":"abc"`) {
		t.Fatalf("output leaked enum 'abc': %s", s)
	}
	// Check that paths collapsed to {*}
	for _, recItem := range rec.Records {
		for _, lf := range recItem.Leaves {
			if strings.Contains(lf.Path, "my secret file") {
				t.Fatalf("path has secret: %s", lf.Path)
			}
			if lf.Enum == "abc" {
				t.Fatalf("enum has abc: %+v", lf)
			}
		}
	}
}

func TestReduceHostileKey(t *testing.T) {
	// A key x');require('child_process') becomes {*}
	body := []byte(`{"x');require('child_process')": 123}`)
	rec := Reduce(body, false, testKey)
	if len(rec.Records) != 1 || len(rec.Records[0].Leaves) != 1 {
		t.Fatalf("unexpected records: %+v", rec)
	}
	lf := rec.Records[0].Leaves[0]
	if lf.Path != "{*}" {
		t.Fatalf("expected path {*}, got %s", lf.Path)
	}
}

func TestReduceSanitizerPropertiesAndSSEEvent(t *testing.T) {
	// Sanitizer: a request with tools[0].input_schema.properties.my_private_tool_arg
	// and the SSE line event: IGNORE_THIS_x stores neither string in any table
	reqBody := []byte(`{"tools":[{"input_schema":{"properties":{"my_private_tool_arg":{"type":"string"}}}}]}`)
	recReq := Reduce(reqBody, false, testKey)
	reqRaw, _ := recReq.MarshalJSON()
	if strings.Contains(string(reqRaw), "my_private_tool_arg") {
		t.Fatalf("sanitizer leaked my_private_tool_arg: %s", string(reqRaw))
	}

	sseBody := []byte("event: IGNORE_THIS_x\ndata: {\"ok\":true}\n\n")
	recSSE := Reduce(sseBody, true, testKey)
	sseRaw, _ := recSSE.MarshalJSON()
	if strings.Contains(string(sseRaw), "IGNORE_THIS_x") {
		t.Fatalf("sanitizer leaked IGNORE_THIS_x: %s", string(sseRaw))
	}
}

func TestReduceAnthropicToolInput(t *testing.T) {
	// messages[0].content[0].input = {"my_private_tool_arg":1,"status":"/home/x/secret"} stores neither string in any table
	body := []byte(`{"messages":[{"content":[{"input":{"my_private_tool_arg":1,"status":"/home/x/secret"}}]}]}`)
	rec := Reduce(body, false, testKey)
	raw, _ := rec.MarshalJSON()
	if strings.Contains(string(raw), "my_private_tool_arg") {
		t.Fatalf("leaked my_private_tool_arg: %s", string(raw))
	}
	if strings.Contains(string(raw), "/home/x/secret") {
		t.Fatalf("leaked secret value: %s", string(raw))
	}
}

func TestReduce30DeltasMatchesWhole(t *testing.T) {
	// A value split into 30 deltas matches the same value sent whole.
	const wholeText = "This is a long sentence that will be split into thirty small chunks for testing streaming."
	wholeBody := []byte(`{"content":"` + wholeText + `"}`)
	recWhole := Reduce(wholeBody, false, testKey)
	var wholeHash string
	var wholeLen int
	for _, recItem := range recWhole.Records {
		for _, lf := range recItem.Leaves {
			if lf.Path == "content" {
				wholeHash = lf.Hash
				wholeLen = lf.Len
			}
		}
	}
	if wholeHash == "" || wholeLen != len(wholeText) {
		t.Fatalf("whole text not reduced properly: hash=%s len=%d", wholeHash, wholeLen)
	}

	// Now split into 30 chunks in an SSE stream
	chunkSize := (len(wholeText) + 29) / 30
	var sseBuf bytes.Buffer
	for i := 0; i < len(wholeText); i += chunkSize {
		end := i + chunkSize
		if end > len(wholeText) {
			end = len(wholeText)
		}
		chunk := wholeText[i:end]
		sseBuf.WriteString("event: content_block_delta\ndata: {\"delta\":{\"content\":\"" + chunk + "\"}}\n\n")
	}

	recStream := Reduce(sseBuf.Bytes(), true, testKey)
	var streamHash string
	var streamLen int
	var streamDeltas int
	for _, recItem := range recStream.Records {
		for _, lf := range recItem.Leaves {
			if strings.HasSuffix(lf.Path, "content") {
				streamHash = lf.Hash
				streamLen = lf.Len
				streamDeltas = lf.Deltas
			}
		}
	}
	if streamHash != wholeHash {
		t.Fatalf("stream hash %q != whole hash %q", streamHash, wholeHash)
	}
	if streamLen != wholeLen {
		t.Fatalf("stream len %d != whole len %d", streamLen, wholeLen)
	}
	if streamDeltas == 0 {
		t.Fatalf("expected deltas > 0, got %d", streamDeltas)
	}
}

func TestC10ReduceAnthropicContentBlockDeltaStream(t *testing.T) {
	sseData := `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}

`
	rec := Reduce([]byte(sseData), true, testKey)
	var foundIndex, foundDeltaType, foundDeltaText bool
	var deltaTextLeaf Leaf

	for _, r := range rec.Records {
		if r.Event == "content_block_delta" {
			for _, lf := range r.Leaves {
				if lf.Path == "index" {
					foundIndex = true
				}
				if lf.Path == "delta.type" && lf.Enum == "text_delta" {
					foundDeltaType = true
				}
				if lf.Path == "delta.text" {
					foundDeltaText = true
					deltaTextLeaf = lf
				}
			}
		}
	}

	if !foundIndex {
		t.Fatal("expected index leaf in content_block_delta")
	}
	if !foundDeltaType {
		t.Fatal("expected enum delta.type leaf in content_block_delta")
	}
	if !foundDeltaText {
		t.Fatal("expected accumulated delta.text leaf in content_block_delta")
	}
	if deltaTextLeaf.Deltas != 2 {
		t.Fatalf("expected delta count 2, got %d", deltaTextLeaf.Deltas)
	}
	if deltaTextLeaf.Len != len("Hello world!") {
		t.Fatalf("expected delta.text len %d, got %d", len("Hello world!"), deltaTextLeaf.Len)
	}
}

func TestReduceDepthLimitCut(t *testing.T) {
	// Depth > 12 should emit a leaf with type "cut" and path ending in {cut}
	body := []byte(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":{"k":{"l":{"m":{"n":1}}}}}}}}}}}}}}`)
	rec := Reduce(body, false, testKey)
	foundCut := false
	for _, recItem := range rec.Records {
		for _, lf := range recItem.Leaves {
			if lf.Type == "cut" && strings.HasSuffix(lf.Path, "{cut}") {
				foundCut = true
			}
		}
	}
	if !foundCut {
		t.Fatalf("expected cut leaf at depth > 12, got %+v", rec)
	}
}
