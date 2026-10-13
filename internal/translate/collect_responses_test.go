package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

const codexStream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"encrypted_content\":\"e\",\"summary\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"

// Codex answers only as a stream; a caller that asked for one JSON body gets the
// completed response, with the items the stream sent when the final event lists none.
func TestCollectResponsesStreamBuildsTheCompletedResponse(t *testing.T) {
	body, errMsg := CollectResponsesStream(strings.NewReader(codexStream))
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	var r obj
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	out := list(r["output"])
	if str(r["status"]) != "completed" || len(out) != 2 || str(asObj(out[0])["encrypted_content"]) != "e" || asObj(r["usage"]) == nil {
		t.Fatalf("response = %s", body)
	}
}

func TestCollectResponsesStreamReportsAFailureAndACut(t *testing.T) {
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"boom\"}}}\n\n"
	if _, msg := CollectResponsesStream(strings.NewReader(failed)); !strings.Contains(msg, "boom") {
		t.Fatalf("failed stream: %q", msg)
	}
	cut := strings.SplitN(codexStream, "event: response.completed", 2)[0]
	if _, msg := CollectResponsesStream(strings.NewReader(cut)); msg == "" {
		t.Fatal("a stream with no completed event was taken as an answer")
	}
}
