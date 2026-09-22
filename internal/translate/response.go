package translate

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
)

func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func num(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	}
	return 0
}

// ---- errors

// Error converts an upstream error body to the caller's shape. A body that is
// not a recognised error is wrapped as the message.
func Error(body []byte, to string) []byte {
	msg, typ := string(body), "api_error"
	if m, err := decode(body); err == nil {
		switch e := m["error"].(type) {
		case obj:
			if s := str(e["message"]); s != "" {
				msg = s
			}
			if s := str(e["type"]); s != "" {
				typ = s
			}
		case string:
			msg = e
		}
	}
	if to == Anthropic {
		b, _ := json.Marshal(obj{"type": "error", "error": obj{"type": typ, "message": msg}})
		return b
	}
	b, _ := json.Marshal(obj{"error": obj{"message": msg, "type": typ}})
	return b
}

// ---- whole responses

var anthropicStop = map[string]string{
	"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
	"tool_use": "tool_calls", "pause_turn": "stop", "refusal": "content_filter",
}

var openaiStop = map[string]string{
	"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
	"function_call": "tool_use", "content_filter": "end_turn",
}

// AnthropicResponseToOpenAI converts a Messages response to a Chat Completion.
func AnthropicResponseToOpenAI(body []byte) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	var text, thinking strings.Builder
	var calls []any
	for _, b := range list(in["content"]) {
		blk := asObj(b)
		switch str(blk["type"]) {
		case "text":
			text.WriteString(str(blk["text"]))
		case "thinking":
			thinking.WriteString(str(blk["thinking"]))
		case "tool_use":
			args, _ := json.Marshal(blk["input"])
			calls = append(calls, obj{"id": blk["id"], "type": "function",
				"function": obj{"name": blk["name"], "arguments": string(args)}})
		}
	}
	msg := obj{"role": "assistant", "content": text.String()}
	if thinking.Len() > 0 {
		msg["reasoning_content"] = thinking.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	finish := anthropicStop[str(in["stop_reason"])]
	if finish == "" {
		finish = "stop"
	}
	out := obj{
		"id": "chatcmpl-" + strings.TrimPrefix(str(in["id"]), "msg_"), "object": "chat.completion",
		"created": time.Now().Unix(), "model": in["model"],
		"choices": []any{obj{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if u := asObj(in["usage"]); u != nil {
		out["usage"] = openaiUsage(u)
	}
	return json.Marshal(out)
}

// openaiUsage folds Anthropic's cache counters into the prompt total, which is
// how OpenAI reports them, and keeps the cached part as a detail.
func openaiUsage(u obj) obj {
	cached := num(u["cache_read_input_tokens"])
	in := num(u["input_tokens"]) + cached + num(u["cache_creation_input_tokens"])
	outTok := num(u["output_tokens"])
	o := obj{"prompt_tokens": in, "completion_tokens": outTok, "total_tokens": in + outTok}
	if cached > 0 {
		o["prompt_tokens_details"] = obj{"cached_tokens": cached}
	}
	return o
}

// OpenAIResponseToAnthropic converts a Chat Completion to a Messages response.
func OpenAIResponseToAnthropic(body []byte) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	var content []any
	stop := "end_turn"
	if ch := asObj(firstOf(in["choices"])); ch != nil {
		msg := asObj(ch["message"])
		if t := str(msg["content"]); t != "" {
			content = append(content, obj{"type": "text", "text": t})
		}
		for _, tc := range list(msg["tool_calls"]) {
			t := asObj(tc)
			fn := asObj(t["function"])
			content = append(content, obj{"type": "tool_use", "id": t["id"], "name": fn["name"],
				"input": parseArgs(str(fn["arguments"]))})
		}
		if s := openaiStop[str(ch["finish_reason"])]; s != "" {
			stop = s
		}
	}
	if content == nil {
		content = []any{}
	}
	out := obj{
		"id": "msg_" + strings.TrimPrefix(str(in["id"]), "chatcmpl-"), "type": "message", "role": "assistant",
		"model": in["model"], "content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": anthropicUsage(asObj(in["usage"])),
	}
	return json.Marshal(out)
}

func anthropicUsage(u obj) obj {
	cached := num(asObj(u["prompt_tokens_details"])["cached_tokens"])
	o := obj{"input_tokens": num(u["prompt_tokens"]) - cached, "output_tokens": num(u["completion_tokens"])}
	if cached > 0 {
		o["cache_read_input_tokens"] = cached
	}
	return o
}

func firstOf(v any) any {
	if l := list(v); len(l) > 0 {
		return l[0]
	}
	return nil
}

// ---- streams

// Flusher is the part of an http.ResponseWriter a stream converter needs.
type Flusher interface {
	io.Writer
	Flush() error
}

// sseReader yields the data payloads of a Server-Sent-Events stream, with the
// event name when the stream sets one.
func sseEvents(r io.Reader, each func(event, data string) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 && !each(event, strings.Join(data, "\n")) {
				return
			}
			event, data = "", nil
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if len(data) > 0 {
		each(event, strings.Join(data, "\n"))
	}
}

// AnthropicStreamToOpenAI reads a Messages event stream and writes the
// equivalent Chat Completions chunks.
func AnthropicStreamToOpenAI(dst Flusher, src io.Reader) {
	id, model := newID("chatcmpl-"), ""
	created := time.Now().Unix()
	toolIndex := map[int64]int{} // content block index -> tool call index
	var usage obj
	finish := ""
	emit := func(delta obj, fin any, extra obj) {
		ch := obj{"index": 0, "delta": delta, "finish_reason": fin}
		c := obj{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{ch}}
		for k, v := range extra {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		io.WriteString(dst, "data: "+string(b)+"\n\n")
		dst.Flush()
	}
	sseEvents(src, func(_, data string) bool {
		ev, err := decode([]byte(data))
		if err != nil {
			return true
		}
		switch str(ev["type"]) {
		case "message_start":
			m := asObj(ev["message"])
			model = str(m["model"])
			usage = asObj(m["usage"])
			emit(obj{"role": "assistant", "content": ""}, nil, nil)
		case "content_block_start":
			blk := asObj(ev["content_block"])
			if str(blk["type"]) == "tool_use" {
				i := len(toolIndex)
				toolIndex[num(ev["index"])] = i
				emit(obj{"tool_calls": []any{obj{"index": i, "id": blk["id"], "type": "function",
					"function": obj{"name": blk["name"], "arguments": ""}}}}, nil, nil)
			}
		case "content_block_delta":
			d := asObj(ev["delta"])
			switch str(d["type"]) {
			case "text_delta":
				emit(obj{"content": d["text"]}, nil, nil)
			case "thinking_delta":
				emit(obj{"reasoning_content": d["thinking"]}, nil, nil)
			case "input_json_delta":
				emit(obj{"tool_calls": []any{obj{"index": toolIndex[num(ev["index"])],
					"function": obj{"arguments": d["partial_json"]}}}}, nil, nil)
			}
		case "message_delta":
			if s := anthropicStop[str(asObj(ev["delta"])["stop_reason"])]; s != "" {
				finish = s
			}
			if u := asObj(ev["usage"]); u != nil {
				if usage == nil {
					usage = obj{}
				}
				for k, v := range u {
					usage[k] = v
				}
			}
		case "message_stop":
			if finish == "" {
				finish = "stop"
			}
			var extra obj
			if usage != nil {
				extra = obj{"usage": openaiUsage(usage)}
			}
			emit(obj{}, finish, extra)
			io.WriteString(dst, "data: [DONE]\n\n")
			dst.Flush()
			return false
		case "error":
			b, _ := json.Marshal(obj{"error": asObj(ev["error"])})
			io.WriteString(dst, "data: "+string(b)+"\n\n")
			dst.Flush()
			return false
		}
		return true
	})
}

// OpenAIStreamToAnthropic reads Chat Completions chunks and writes the
// equivalent Messages event stream.
func OpenAIStreamToAnthropic(dst Flusher, src io.Reader) {
	send := func(event string, v obj) {
		b, _ := json.Marshal(v)
		io.WriteString(dst, "event: "+event+"\ndata: "+string(b)+"\n\n")
		dst.Flush()
	}
	started := false
	block := -1     // index of the open content block, -1 when none
	blockKind := "" // "text" or "tool"
	toolBlock := map[int64]int{}
	stop := ""
	var usage obj
	start := func(id, model string) {
		if started {
			return
		}
		started = true
		send("message_start", obj{"type": "message_start", "message": obj{
			"id": "msg_" + strings.TrimPrefix(id, "chatcmpl-"), "type": "message", "role": "assistant",
			"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": obj{"input_tokens": 0, "output_tokens": 0}}})
	}
	closeBlock := func() {
		if block >= 0 {
			send("content_block_stop", obj{"type": "content_block_stop", "index": block})
		}
	}
	next := 0
	open := func(kind string, cb obj) int {
		closeBlock()
		block, blockKind = next, kind
		next++
		send("content_block_start", obj{"type": "content_block_start", "index": block, "content_block": cb})
		return block
	}
	finish := func() {
		start(newID("chatcmpl-"), "")
		closeBlock()
		block = -1
		if stop == "" {
			stop = "end_turn"
		}
		u := obj{"output_tokens": 0}
		if usage != nil {
			u = anthropicUsage(usage)
		}
		send("message_delta", obj{"type": "message_delta", "delta": obj{"stop_reason": stop, "stop_sequence": nil}, "usage": u})
		send("message_stop", obj{"type": "message_stop"})
	}
	done := false
	sseEvents(src, func(_, data string) bool {
		if strings.TrimSpace(data) == "[DONE]" {
			finish()
			done = true
			return false
		}
		ch, err := decode([]byte(data))
		if err != nil {
			return true
		}
		if e := asObj(ch["error"]); e != nil {
			send("error", obj{"type": "error", "error": obj{"type": "api_error", "message": str(e["message"])}})
			done = true
			return false
		}
		start(str(ch["id"]), str(ch["model"]))
		if u := asObj(ch["usage"]); u != nil {
			usage = u
		}
		c := asObj(firstOf(ch["choices"]))
		if c == nil {
			return true
		}
		d := asObj(c["delta"])
		if t := str(d["content"]); t != "" {
			if blockKind != "text" || block < 0 {
				open("text", obj{"type": "text", "text": ""})
			}
			send("content_block_delta", obj{"type": "content_block_delta", "index": block,
				"delta": obj{"type": "text_delta", "text": t}})
		}
		for _, raw := range list(d["tool_calls"]) {
			tc := asObj(raw)
			ti := num(tc["index"])
			fn := asObj(tc["function"])
			bi, seen := toolBlock[ti]
			if !seen {
				bi = open("tool", obj{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": obj{}})
				toolBlock[ti] = bi
			}
			if a := str(fn["arguments"]); a != "" && bi == block {
				send("content_block_delta", obj{"type": "content_block_delta", "index": bi,
					"delta": obj{"type": "input_json_delta", "partial_json": a}})
			}
		}
		if s := openaiStop[str(c["finish_reason"])]; s != "" {
			stop = s
		}
		return true
	})
	if !done {
		finish()
	}
}
