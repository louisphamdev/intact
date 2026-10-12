package servertools

import (
	"bytes"
	"encoding/json"
)

// MessageSSE renders a finished message as the event stream Anthropic's API
// sends, for a client that asked for a stream while intact ran tools between the
// calls to the model. The whole message arrives at once: the events are those of
// a stream, in their order, with one delta per block.
func MessageSSE(msg map[string]any) []byte {
	var b bytes.Buffer
	send := func(event string, data obj) {
		raw, _ := json.Marshal(data)
		b.WriteString("event: " + event + "\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	usage := asObj(msg["usage"])
	start := obj{"id": msg["id"], "type": "message", "role": "assistant", "model": msg["model"],
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage(usage)}
	send("message_start", obj{"type": "message_start", "message": start})

	for i, raw := range list(msg["content"]) {
		blk := asObj(raw)
		var open obj
		var deltas []obj
		switch str(blk["type"]) {
		case "text":
			open = obj{"type": "text", "text": ""}
			if t := str(blk["text"]); t != "" {
				deltas = append(deltas, obj{"type": "text_delta", "text": t})
			}
		case "thinking":
			open = obj{"type": "thinking", "thinking": "", "signature": ""}
			if t := str(blk["thinking"]); t != "" {
				deltas = append(deltas, obj{"type": "thinking_delta", "thinking": t})
			}
			if s := str(blk["signature"]); s != "" {
				deltas = append(deltas, obj{"type": "signature_delta", "signature": s})
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			open = copyObj(blk)
			input, _ := json.Marshal(blk["input"])
			open["input"] = obj{}
			if string(input) != "{}" && string(input) != "null" {
				deltas = append(deltas, obj{"type": "input_json_delta", "partial_json": string(input)})
			}
		default:
			open = blk // a result block arrives whole
		}
		send("content_block_start", obj{"type": "content_block_start", "index": i, "content_block": open})
		for _, d := range deltas {
			send("content_block_delta", obj{"type": "content_block_delta", "index": i, "delta": d})
		}
		send("content_block_stop", obj{"type": "content_block_stop", "index": i})
	}

	out := obj{"output_tokens": number(usage["output_tokens"])}
	if s := usage["server_tool_use"]; s != nil {
		out["server_tool_use"] = s
	}
	send("message_delta", obj{"type": "message_delta",
		"delta": obj{"stop_reason": msg["stop_reason"], "stop_sequence": msg["stop_sequence"]}, "usage": out})
	send("message_stop", obj{"type": "message_stop"})
	return b.Bytes()
}

// startUsage is the usage message_start carries: what the request cost, with the
// output still to come.
func startUsage(u obj) obj {
	out := obj{}
	for k, v := range u {
		if k != "output_tokens" && k != "server_tool_use" {
			out[k] = v
		}
	}
	out["output_tokens"] = 1
	return out
}
