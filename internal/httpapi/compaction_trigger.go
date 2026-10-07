package httpapi

import (
	"bytes"
	"encoding/json"
	"log"

	"strings"
)

// Codex remote compaction.
//
// Codex 0.160 asks for compaction with an ordinary Responses request whose last input item is
// {"type":"compaction_trigger"}, and its remote compaction v2 accepts the answer only if it
// carries exactly one output item of type "compaction". A provider that answers as an ordinary
// completion gives zero of them, and Codex treats that as a fatal error raised *before* it
// replaces the history: the thread stays exactly as long as it was, so every resume compacts
// again and dies the same way. The feature is reported as removed and cannot be switched off,
// so there is no configuration that avoids it, and the thread is unrecoverable once it happens.
//
// The contract on this side is small. When the last input item is a compaction_trigger, answer
// with one output item:
//
//	{"type":"compaction","id":"<id>","encrypted_content":"<the summary>"}
//
// encrypted_content is opaque to Codex: it is stored in the history as the summary item and
// never decoded, so the summary may be plain text. What is not negotiable is the item count --
// one, and of that type -- because the check is on the count.
//
// So the trigger is answered here rather than forwarded as it stands. intact asks the provider
// for the summary as an ordinary completion and then answers Codex in the shape it demanded.
// That keeps the thread alive, and because the summary replaces the history, it keeps the next
// turn inside its window.

// compactionTrigger reports whether a Responses body asks for compaction. The trigger is the
// last input item, which is where Codex puts it.
func compactionTrigger(body []byte) bool {
	items, ok := responsesInput(body)
	if !ok || len(items) == 0 {
		return false
	}
	var last struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(items[len(items)-1], &last) != nil {
		return false
	}
	return last.Type == "compaction_trigger"
}

// responsesInput reads the input items of a Responses body.
func responsesInput(body []byte) ([]json.RawMessage, bool) {
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil || req.Input == nil {
		return nil, false
	}
	return req.Input, true
}

// compactionInstruction asks for the summary the trigger is asking for. It replaces the
// trigger, so the provider is asked a question rather than handed an item type it has never
// seen.
const compactionInstruction = "Summarize this conversation so it can replace the history. " +
	"Keep: what was asked, the decisions made and why, the files and commands that changed things, " +
	"the errors and how they were resolved, and what is still pending. Drop: full tool output, " +
	"intermediate reasoning, and anything already superseded. Write plain prose a continuing " +
	"agent can act on without the original transcript."

// summaryRequest is the request to send in place of the trigger: the same request with the
// trigger dropped and an instruction appended, and everything else left exactly as the client
// wrote it.
//
// The rest of the body is carried over rather than rebuilt, because providers put requirements
// on it that this feature has no business second-guessing. OpenAI's Responses, which is what
// Codex talks to, refuses a request that does not set store to false, and refuses one that does
// not set stream to true: it answers that account's every other turn and would refuse this one.
// Reasoning settings decide whether the summary comes back with the reasoning the client asked
// to keep. So store, stream, include and reasoning all pass through, and the answer is read as
// whatever shape they asked for.
func summaryRequest(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	items, ok := responsesInput(body)
	if !ok || len(items) == 0 {
		return nil
	}
	kept := make([]json.RawMessage, 0, len(items))
	kept = append(kept, items[:len(items)-1]...)
	msg, err := marshalPlain(map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": compactionInstruction}},
	})
	if err != nil {
		return nil
	}
	kept = append(kept, msg)
	input, err := marshalPlain(kept)
	if err != nil {
		return nil
	}
	req["input"] = input
	// The model is not set here. The body already carries the one intact rewrote for this
	// account, without the provider prefix the client sent; putting the caller's own name
	// back would send a model the account does not serve.
	out, err := marshalPlain(req)
	if err != nil {
		return nil
	}
	return out
}

// compactionItem is the one output item a compaction answer carries.
func compactionItem(id, summary string) map[string]any {
	return map[string]any{"type": "compaction", "id": id, "encrypted_content": summary}
}

// compactionBody is the whole-answer form of a compaction reply.
func compactionBody(id, respID, summary, model string) []byte {
	b, _ := marshalPlain(map[string]any{
		"id":     respID,
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": []any{compactionItem(id, summary)},
	})
	return b
}

// compactionStream is the streamed form. Codex collects the item from
// response.output_item.done and then reads the completed response, so both name the same single
// item -- which is also why it must not be counted twice.
func compactionStream(id, respID, summary, model string) []byte {
	item := compactionItem(id, summary)
	events := []map[string]any{
		{"type": "response.created", "sequence_number": 0,
			"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model}},
		{"type": "response.output_item.done", "output_index": 0, "sequence_number": 1, "item": item},
		{"type": "response.completed", "sequence_number": 2,
			"response": map[string]any{"id": respID, "object": "response", "status": "completed",
				"model": model, "output": []any{item}}},
	}
	var buf bytes.Buffer
	for _, ev := range events {
		b, err := marshalPlain(ev)
		if err != nil {
			continue
		}
		buf.WriteString("event: ")
		buf.WriteString(ev["type"].(string))
		buf.WriteString("\ndata: ")
		buf.Write(b)
		buf.WriteString("\n\n")
	}
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

// compactionSummary pulls the text out of an ordinary answer, in any of the shapes intact
// relays. A provider that ignored the instruction and answered with a tool call has nothing to
// store, which is reported rather than stored empty: an empty summary would replace the history
// with nothing, which is worse than the failure it replaces.
func compactionSummary(body []byte) string {
	var whole struct {
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Text    string `json:"text"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &whole) != nil {
		return ""
	}
	for _, item := range whole.Output {
		// A call has no summary in it. Returning "" is the honest answer, and the caller
		// reports it rather than replacing the history with a blank item.
		if item.CallID != "" {
			return ""
		}
		if t := strings.TrimSpace(item.Text); t != "" {
			return t
		}
		var b strings.Builder
		for _, c := range item.Content {
			b.WriteString(c.Text)
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			return t
		}
	}
	for _, ch := range whole.Choices {
		if t := strings.TrimSpace(ch.Message.Content); t != "" {
			return t
		}
	}
	for _, cand := range whole.Candidates {
		var b strings.Builder
		for _, p := range cand.Content.Parts {
			b.WriteString(p.Text)
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			return t
		}
	}
	return ""
}

// compactionSummaryFromStream reads the text out of a streamed answer, which is what a provider
// sends when it streams whatever it was asked for.
func compactionSummaryFromStream(body []byte) string {
	var b strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var ev struct {
			Type    string `json:"type"`
			Delta   string `json:"delta"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Response struct {
				Output []struct {
					Text    string `json:"text"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			b.WriteString(ev.Delta)
		case "response.output_item.done", "response.completed":
			for _, item := range ev.Response.Output {
				b.WriteString(item.Text)
				for _, c := range item.Content {
					b.WriteString(c.Text)
				}
			}
		default:
			for _, ch := range ev.Choices {
				b.WriteString(ch.Delta.Content)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// compactionIDs are the ids of the two items in one compaction reply. They are separate because
// Codex treats the output_item.done item and the response object as two places the same single
// item is named, and a shared id would read as the item appearing twice.
func compactionIDs() (item, response string) {
	n := newRequestID()
	return "cmp_" + n, "resp_" + n
}

// logCompaction says what was done, because a compaction looks like an ordinary turn in the
// transcript and a thread that compacts silently is hard to reason about afterwards.
func logCompaction(prov, model string, before, after int) {
	log.Printf("compaction: %s summarized %s for codex, %d -> %d bytes", prov, model, before, after)
}

func logCompactionFailed(prov, model, why string) {
	log.Printf("compaction: %s gave no summary for %s (%s)", prov, model, why)
}
