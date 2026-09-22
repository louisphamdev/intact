// Package translate converts between the two request shapes intact serves at
// /v1: OpenAI Chat Completions and Anthropic Messages. It is used only when the
// caller's shape differs from the provider's; a request that already matches is
// forwarded untouched.
//
// Numbers are decoded as json.Number and schemas are carried as they are, so a
// value that does not need converting keeps its exact text.
package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Shapes of a request or response.
const (
	OpenAI    = "openai"
	Anthropic = "anthropic"
)

// DefaultMaxTokens fills Anthropic's required max_tokens when an OpenAI caller
// sends none.
const DefaultMaxTokens = 8192

type obj = map[string]any

func decode(b []byte) (obj, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m obj
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("body is not a JSON object")
	}
	return m, nil
}

func str(v any) string { s, _ := v.(string); return s }

func list(v any) []any { l, _ := v.([]any); return l }

func asObj(v any) obj { m, _ := v.(obj); return m }

// Stream reports whether a request asks for a streamed answer.
func Stream(body []byte) bool {
	m, err := decode(body)
	if err != nil {
		return false
	}
	b, _ := m["stream"].(bool)
	return b
}

// ---- OpenAI Chat Completions -> Anthropic Messages

// OpenAIToAnthropic converts a Chat Completions request to a Messages request.
func OpenAIToAnthropic(body []byte) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	out := obj{"model": in["model"]}

	var system []any
	var msgs []obj
	for _, raw := range list(in["messages"]) {
		m := asObj(raw)
		switch role := str(m["role"]); role {
		case "system", "developer":
			for _, b := range oaContentToBlocks(m["content"]) {
				if asObj(b)["type"] == "text" {
					system = append(system, b)
				}
			}
		case "user":
			msgs = appendMsg(msgs, "user", oaContentToBlocks(m["content"]))
		case "assistant":
			blocks := oaContentToBlocks(m["content"])
			for _, tc := range list(m["tool_calls"]) {
				t := asObj(tc)
				fn := asObj(t["function"])
				blocks = append(blocks, obj{"type": "tool_use", "id": t["id"], "name": fn["name"],
					"input": parseArgs(str(fn["arguments"]))})
			}
			msgs = appendMsg(msgs, "assistant", blocks)
		case "tool":
			res := obj{"type": "tool_result", "tool_use_id": m["tool_call_id"]}
			if c := oaContentToBlocks(m["content"]); len(c) > 0 {
				res["content"] = c
			}
			msgs = appendMsg(msgs, "user", []any{res})
		default:
			return nil, fmt.Errorf("unsupported message role %q", role)
		}
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = msgs

	switch {
	case in["max_tokens"] != nil:
		out["max_tokens"] = in["max_tokens"]
	case in["max_completion_tokens"] != nil:
		out["max_tokens"] = in["max_completion_tokens"]
	default:
		out["max_tokens"] = DefaultMaxTokens
	}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	switch s := in["stop"].(type) {
	case string:
		out["stop_sequences"] = []any{s}
	case []any:
		out["stop_sequences"] = s
	}
	if u := str(in["user"]); u != "" {
		out["metadata"] = obj{"user_id": u}
	}
	if tools := list(in["tools"]); len(tools) > 0 {
		var ts []any
		for _, t := range tools {
			fn := asObj(asObj(t)["function"])
			if fn == nil {
				continue
			}
			schema := fn["parameters"]
			if schema == nil {
				schema = obj{"type": "object", "properties": obj{}}
			}
			td := obj{"name": fn["name"], "input_schema": schema}
			if d := str(fn["description"]); d != "" {
				td["description"] = d
			}
			ts = append(ts, td)
		}
		out["tools"] = ts
	}
	if tc := oaToolChoice(in["tool_choice"]); tc != nil {
		if p, ok := in["parallel_tool_calls"].(bool); ok && !p {
			tc["disable_parallel_tool_use"] = true
		}
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// appendMsg adds content under role, merging into the previous message when it
// has the same role: Anthropic expects user and assistant to alternate, and an
// OpenAI conversation puts each tool result in a message of its own.
func appendMsg(msgs []obj, role string, blocks []any) []obj {
	if len(blocks) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
		msgs[n-1]["content"] = append(msgs[n-1]["content"].([]any), blocks...)
		return msgs
	}
	return append(msgs, obj{"role": role, "content": blocks})
}

// oaContentToBlocks turns OpenAI message content (a string or a list of parts)
// into Anthropic content blocks.
func oaContentToBlocks(c any) []any {
	switch v := c.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []any{obj{"type": "text", "text": v}}
	case []any:
		var out []any
		for _, p := range v {
			part := asObj(p)
			switch str(part["type"]) {
			case "text", "input_text":
				if t := str(part["text"]); t != "" {
					out = append(out, obj{"type": "text", "text": t})
				}
			case "image_url":
				u := str(asObj(part["image_url"])["url"])
				if u == "" {
					u = str(part["image_url"])
				}
				if img := imageBlock(u); img != nil {
					out = append(out, img)
				}
			}
		}
		return out
	}
	return nil
}

// imageBlock converts an image URL, either a data: URL or a web URL.
func imageBlock(u string) obj {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		media, isB64 := strings.CutSuffix(meta, ";base64")
		if !found || !isB64 {
			return nil
		}
		return obj{"type": "image", "source": obj{"type": "base64", "media_type": media, "data": data}}
	}
	if u == "" {
		return nil
	}
	return obj{"type": "image", "source": obj{"type": "url", "url": u}}
}

// parseArgs reads a tool call's arguments. Anthropic wants an object; an empty
// or broken string becomes an empty object rather than failing the request.
func parseArgs(s string) any {
	if strings.TrimSpace(s) == "" {
		return obj{}
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return obj{}
	}
	if _, ok := v.(obj); !ok {
		return obj{}
	}
	return v
}

func oaToolChoice(v any) obj {
	switch c := v.(type) {
	case string:
		switch c {
		case "auto":
			return obj{"type": "auto"}
		case "none":
			return obj{"type": "none"}
		case "required":
			return obj{"type": "any"}
		}
	case obj:
		if name := str(asObj(c["function"])["name"]); name != "" {
			return obj{"type": "tool", "name": name}
		}
	}
	return nil
}

// ---- Anthropic Messages -> OpenAI Chat Completions

// AnthropicToOpenAI converts a Messages request to a Chat Completions request.
func AnthropicToOpenAI(body []byte) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	out := obj{"model": in["model"]}
	var msgs []any

	switch s := in["system"].(type) {
	case string:
		if s != "" {
			msgs = append(msgs, obj{"role": "system", "content": s})
		}
	case []any:
		if t := joinText(s); t != "" {
			msgs = append(msgs, obj{"role": "system", "content": t})
		}
	}
	for _, raw := range list(in["messages"]) {
		m := asObj(raw)
		role := str(m["role"])
		blocks, isList := m["content"].([]any)
		if !isList {
			msgs = append(msgs, obj{"role": role, "content": str(m["content"])})
			continue
		}
		var parts []any
		var calls []any
		for _, b := range blocks {
			blk := asObj(b)
			switch str(blk["type"]) {
			case "text":
				parts = append(parts, obj{"type": "text", "text": blk["text"]})
			case "image":
				if u := imageURL(asObj(blk["source"])); u != "" {
					parts = append(parts, obj{"type": "image_url", "image_url": obj{"url": u}})
				}
			case "tool_use":
				args, _ := json.Marshal(blk["input"])
				calls = append(calls, obj{"id": blk["id"], "type": "function",
					"function": obj{"name": blk["name"], "arguments": string(args)}})
			case "tool_result":
				// A tool result is its own message in OpenAI, and it must follow
				// the assistant turn that called the tool, before any user text.
				content := blk["content"]
				if l, ok := content.([]any); ok {
					content = joinText(l)
				}
				if e, _ := blk["is_error"].(bool); e {
					content = "Error: " + str(content)
				}
				msgs = append(msgs, obj{"role": "tool", "tool_call_id": blk["tool_use_id"], "content": str(content)})
			}
		}
		if role == "assistant" {
			msg := obj{"role": "assistant", "content": joinParts(parts)}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			if msg["content"] != nil || len(calls) > 0 {
				msgs = append(msgs, msg)
			}
			continue
		}
		if len(parts) > 0 {
			msgs = append(msgs, obj{"role": role, "content": simplifyParts(parts)})
		}
	}
	out["messages"] = msgs

	for _, k := range []string{"max_tokens", "temperature", "top_p"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if s, ok := in["stream"].(bool); ok {
		out["stream"] = s
		if s {
			// The usage of a stream arrives only when asked for.
			out["stream_options"] = obj{"include_usage": true}
		}
	}
	if ss := list(in["stop_sequences"]); len(ss) > 0 {
		out["stop"] = ss
	}
	if u := str(asObj(in["metadata"])["user_id"]); u != "" {
		out["user"] = u
	}
	if tools := list(in["tools"]); len(tools) > 0 {
		var ts []any
		for _, t := range tools {
			td := asObj(t)
			if td["input_schema"] == nil {
				continue // a server tool (web search, …) has no OpenAI counterpart
			}
			fn := obj{"name": td["name"], "parameters": td["input_schema"]}
			if d := str(td["description"]); d != "" {
				fn["description"] = d
			}
			ts = append(ts, obj{"type": "function", "function": fn})
		}
		if len(ts) > 0 {
			out["tools"] = ts
		}
	}
	if tc := asObj(in["tool_choice"]); tc != nil {
		switch str(tc["type"]) {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = obj{"type": "function", "function": obj{"name": tc["name"]}}
		}
		if d, _ := tc["disable_parallel_tool_use"].(bool); d && out["tools"] != nil {
			out["parallel_tool_calls"] = false
		}
	}
	return json.Marshal(out)
}

func imageURL(src obj) string {
	switch str(src["type"]) {
	case "base64":
		return "data:" + str(src["media_type"]) + ";base64," + str(src["data"])
	case "url":
		return str(src["url"])
	}
	return ""
}

// joinText concatenates the text blocks of a list, or returns a plain string.
func joinText(l []any) string {
	var b strings.Builder
	for _, x := range l {
		if s, ok := x.(string); ok {
			b.WriteString(s)
			continue
		}
		if blk := asObj(x); str(blk["type"]) == "text" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(str(blk["text"]))
		}
	}
	return b.String()
}

// joinParts returns an assistant's text, or nil when it has none.
func joinParts(parts []any) any {
	if len(parts) == 0 {
		return nil
	}
	return joinText(parts)
}

// simplifyParts sends text-only content as a plain string, which every
// OpenAI-compatible server accepts; content with an image stays a list.
func simplifyParts(parts []any) any {
	for _, p := range parts {
		if str(asObj(p)["type"]) != "text" {
			return parts
		}
	}
	return joinText(parts)
}
