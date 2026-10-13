package translate

import (
	"encoding/json"
	"io"
)

// CollectResponsesStream folds a Responses event stream into the one response
// body a non-streaming caller asked for. The final event's response is kept;
// when it lists no output, the items of response.output_item.done fill it, in
// stream order. A failure, an error event or a stream with no final event gives
// "" and the reason.
func CollectResponsesStream(src io.Reader) ([]byte, string) {
	var items []any
	var final obj
	reason := ""
	err := sseEvents(src, func(_, data string) bool {
		ev, err := decode([]byte(data))
		if err != nil {
			return true
		}
		switch str(ev["type"]) {
		case "response.output_item.done":
			if it := asObj(ev["item"]); it != nil {
				items = append(items, it)
			}
		case "response.completed", "response.incomplete":
			final = asObj(ev["response"])
			return false
		case "response.failed":
			reason = str(asObj(asObj(ev["response"])["error"])["message"])
			if reason == "" {
				reason = "the upstream response failed"
			}
			return false
		case "error":
			reason = firstText(str(asObj(ev["error"])["message"]), str(ev["message"]), "the upstream stream reported an error")
			return false
		}
		return true
	})
	if reason != "" {
		return nil, reason
	}
	if final == nil {
		if err != nil {
			return nil, "upstream stream ended early: " + err.Error()
		}
		return nil, "upstream stream ended without response.completed"
	}
	if len(list(final["output"])) == 0 && len(items) > 0 {
		final["output"] = items
	}
	b, merr := json.Marshal(final)
	if merr != nil {
		return nil, merr.Error()
	}
	return b, ""
}

func firstText(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
