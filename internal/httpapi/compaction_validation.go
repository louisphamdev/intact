package httpapi

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A history replacement must end successfully. EOF and a partial text delta are not completion.
func completeSummaryResponse(raw []byte) bool {
	var whole map[string]any
	if json.Unmarshal(raw, &whole) == nil {
		return summaryWholeComplete(whole)
	}
	terminal := false
	anthropic := false
	anthropicFinished := false
	anthropicStopped := false
	for _, data := range summarySSEData(raw) {
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(data, &ev) != nil {
			return false
		}
		if ev["error"] != nil {
			return false
		}
		switch ev["type"] {
		case "error", "response.failed", "response.incomplete":
			return false
		case "response.completed":
			r, _ := ev["response"].(map[string]any)
			if !summaryWholeComplete(r) {
				return false
			}
			terminal = true
		case "response.output_item.added", "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			if summaryHasCall(item) {
				return false
			}
		case "content_block_start":
			anthropic = true
			block, _ := ev["content_block"].(map[string]any)
			if summaryHasCall(block) {
				return false
			}
		case "message_delta":
			anthropic = true
			delta, _ := ev["delta"].(map[string]any)
			if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
				if reason != "end_turn" && reason != "stop_sequence" {
					return false
				}
				anthropicFinished = true
			}
		case "message_stop":
			anthropicStopped = true
		}
		if choices, ok := ev["choices"].([]any); ok {
			for _, rawChoice := range choices {
				choice, _ := rawChoice.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				if summaryHasCall(delta) {
					return false
				}
				if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
					if reason != "stop" {
						return false
					}
					terminal = true
				}
			}
		}
		if candidates, ok := ev["candidates"].([]any); ok {
			for _, candidate := range candidates {
				c, _ := candidate.(map[string]any)
				if summaryHasCall(c) {
					return false
				}
				if reason, ok := c["finishReason"].(string); ok && reason != "" {
					if reason != "STOP" {
						return false
					}
					terminal = true
				}
			}
		}
	}
	if anthropic {
		return anthropicFinished && anthropicStopped
	}
	return terminal
}

func summaryHasCall(v any) bool {
	switch value := v.(type) {
	case map[string]any:
		if typ, _ := value["type"].(string); strings.Contains(typ, "call") || strings.Contains(typ, "tool_use") {
			return true
		}
		for k, item := range value {
			if (k == "tool_calls" || k == "function_call" || k == "functionCall") && item != nil {
				if list, ok := item.([]any); !ok || len(list) > 0 {
					return true
				}
			}
			if summaryHasCall(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if summaryHasCall(item) {
				return true
			}
		}
	}
	return false
}

func summaryWholeComplete(whole map[string]any) bool {
	if whole == nil || whole["error"] != nil || summaryHasCall(whole) {
		return false
	}
	if _, ok := whole["output"]; ok {
		return whole["status"] == "completed"
	}
	if choices, ok := whole["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		return choice["finish_reason"] == "stop"
	}
	if candidates, ok := whole["candidates"].([]any); ok && len(candidates) > 0 {
		candidate, _ := candidates[0].(map[string]any)
		return candidate["finishReason"] == "STOP"
	}
	return whole["stop_reason"] == "end_turn" || whole["stop_reason"] == "stop_sequence"
}

func summarySSEData(raw []byte) [][]byte {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	var events [][]byte
	for _, frame := range bytes.Split(raw, []byte("\n\n")) {
		var data [][]byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
			}
		}
		if len(data) > 0 {
			events = append(events, bytes.Join(data, []byte("\n")))
		}
	}
	return events
}
