package translate

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// A Responses caller (the Codex CLI) reaches a provider through the chat hub.
// Chat providers know only function tools, so the Responses tool kinds are
// flattened and restored on the way back. Ported from llm-switcher formats.mjs.

// ToolMeta is the Responses tool behind one flattened chat tool name.
type ToolMeta struct {
	Kind      string // "function", "custom" or "local_shell"
	Name      string
	Namespace string
}

var customToolParams = obj{
	"type":                 "object",
	"properties":           obj{"input": obj{"type": "string", "description": "The raw freeform tool input (not JSON-encoded)."}},
	"required":             []any{"input"},
	"additionalProperties": false,
}

var localShellParams = obj{
	"type": "object",
	"properties": obj{
		"command":    obj{"type": "array", "items": obj{"type": "string"}, "description": `Command and arguments, e.g. ["bash", "-lc", "ls -la"].`},
		"workdir":    obj{"type": "string", "description": "Working directory for the command."},
		"timeout_ms": obj{"type": "number", "description": "Timeout in milliseconds."},
	},
	"required": []any{"command"},
}

// missingToolResult answers a call whose result the history does not hold; a
// chat provider refuses a tool call left without an answer.
const missingToolResult = "[no result: the tool call was interrupted]"

var badToolChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// responsesToolName is the chat name of a Responses tool: chat names allow
// [A-Za-z0-9_-] and 64 characters.
func responsesToolName(namespace, name string) string {
	raw := name
	if namespace != "" {
		raw = namespace + "__" + name
	}
	raw = badToolChar.ReplaceAllString(raw, "_")
	if len(raw) > 64 {
		raw = raw[:64]
	}
	return raw
}

func customToolDescription(t obj) string {
	d := str(t["description"])
	if d != "" {
		d += "\n\n"
	}
	d += `This is a FREEFORM tool: pass the raw input text in the "input" string argument, exactly as the tool expects it (do not wrap it in JSON).`
	if f := asObj(t["format"]); str(f["type"]) == "grammar" && str(f["definition"]) != "" {
		d += "\nThe input must match this " + str(f["syntax"]) + " grammar:\n" + str(f["definition"])
	}
	return d
}

// responsesOutputText flattens a tool output: a string, or a list of content items.
func responsesOutputText(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case []any:
		var parts []string
		for _, x := range o {
			switch p := x.(type) {
			case string:
				parts = append(parts, p)
			case obj:
				if t, ok := p["text"].(string); ok {
					parts = append(parts, t)
				} else if str(p["type"]) == "input_image" {
					parts = append(parts, "[image]")
				} else {
					b, _ := json.Marshal(p)
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func marshalString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// responsesToolList is payload.tools plus the tools of additional_tools items.
func responsesToolList(in obj) []any {
	tools := append([]any(nil), list(in["tools"])...)
	for _, raw := range list(in["input"]) {
		if it := asObj(raw); str(it["type"]) == "additional_tools" {
			tools = append(tools, list(it["tools"])...)
		}
	}
	return tools
}

// walkResponsesTools calls each for every tool a chat provider can run, once
// per chat name. Hosted tools (web_search, tool_search, …) run at OpenAI only.
func walkResponsesTools(tools []any, each func(chatName string, meta ToolMeta, t obj)) {
	seen := map[string]bool{}
	var add func(t obj, namespace string)
	add = func(t obj, namespace string) {
		switch typ := str(t["type"]); typ {
		case "namespace":
			for _, inner := range list(t["tools"]) {
				add(asObj(inner), str(t["name"]))
			}
		case "function", "custom":
			if str(t["name"]) == "" {
				return
			}
			name := responsesToolName(namespace, str(t["name"]))
			if !seen[name] {
				seen[name] = true
				each(name, ToolMeta{Kind: typ, Name: str(t["name"]), Namespace: namespace}, t)
			}
		case "local_shell":
			if !seen["local_shell"] {
				seen["local_shell"] = true
				each("local_shell", ToolMeta{Kind: "local_shell", Name: "local_shell"}, t)
			}
		}
	}
	for _, t := range tools {
		add(asObj(t), "")
	}
}

// ResponsesTools maps each chat tool name that ResponsesToOpenAI declares to
// the Responses tool the caller declared. vocab is the same translation the request goes out with, so
// the key is the name the provider answers and the value still carries the caller's own.
// ResponsesToolNames reads the function tool names a Responses request declares, skipping the custom
// tools and the harness's own local_shell: neither is renamed, so neither is a rename target.
func ResponsesToolNames(body []byte) []string {
	in, err := decode(body)
	if err != nil {
		return nil
	}
	var out []string
	walkResponsesTools(responsesToolList(in), func(name string, meta ToolMeta, _ obj) {
		if meta.Kind == "function" {
			out = append(out, name)
		}
	})
	return out
}

// ResponsesTools maps each chat tool name that ResponsesToOpenAI declares to
// the Responses tool the caller declared. vocab is the same translation the request goes out with, so
// the key is the name the provider answers and the value still carries the caller's own.
func ResponsesTools(body []byte, vocab *ToolNames) map[string]ToolMeta {
	in, err := decode(body)
	if err != nil {
		return nil
	}
	out := map[string]ToolMeta{}
	walkResponsesTools(responsesToolList(in), func(name string, meta ToolMeta, _ obj) {
		// A custom tool carries a grammar written for its name, and local_shell is the harness's own:
		// neither is renamed, so their key is the name they already have.
		key := name
		if meta.Kind == "function" {
			key = vocab.ToProvider(name)
		}
		out[key] = meta
	})
	return out
}

// ResponsesToOpenAI converts a Responses request to a Chat Completions request. vocab translates the
// function tool names to the vocabulary the provider is going to read; custom tools and the harness's
// own local_shell keep theirs, because a freeform tool's grammar is written for its name.
func ResponsesToOpenAI(body []byte, vocab *ToolNames) ([]byte, error) {
	var expandErr error
	body, expandErr = ExpandGatewayCompactions(body, false)
	if expandErr != nil {
		return nil, expandErr
	}
	if vocab == nil {
		vocab = &ToolNames{}
	}
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	var system []string
	if s := str(in["instructions"]); s != "" {
		system = append(system, s)
	}
	var msgs []obj
	last := func() obj {
		if len(msgs) == 0 {
			return nil
		}
		return msgs[len(msgs)-1]
	}
	pushText := func(role, text string) {
		if text == "" {
			return
		}
		if l := last(); l != nil && str(l["role"]) == role && l["tool_calls"] == nil {
			if s, ok := l["content"].(string); ok {
				l["content"] = s + "\n" + text
				return
			}
		}
		msgs = append(msgs, obj{"role": role, "content": text})
	}
	// Parallel calls arrive as separate items; a chat provider wants them in one
	// assistant turn, answered right after it.
	pushCall := func(id, name, args string) {
		call := obj{"id": id, "type": "function", "function": obj{"name": name, "arguments": args}}
		if l := last(); l != nil && str(l["role"]) == "assistant" {
			l["tool_calls"] = append(list(l["tool_calls"]), call)
			return
		}
		msgs = append(msgs, obj{"role": "assistant", "content": nil, "tool_calls": []any{call}})
	}
	callID := func(it obj) string {
		if s := str(it["call_id"]); s != "" {
			return s
		}
		return str(it["id"])
	}

	switch input := in["input"].(type) {
	case string:
		pushText("user", input)
	case []any:
		for _, raw := range input {
			if s, ok := raw.(string); ok {
				pushText("user", s)
				continue
			}
			it := asObj(raw)
			switch typ := str(it["type"]); {
			case typ == "message" || (typ == "" && it["role"] != nil):
				parts, allText := responsesContent(it["content"])
				role := str(it["role"])
				if role == "system" || role == "developer" {
					if t := joinText(parts); t != "" {
						system = append(system, t)
					}
					continue
				}
				if role != "assistant" {
					role = "user"
				}
				if allText {
					pushText(role, joinText(parts))
				} else if len(parts) > 0 {
					msgs = append(msgs, obj{"role": role, "content": parts})
				}
			case typ == "function_call":
				args := str(it["arguments"])
				if args == "" {
					args = "{}"
				}
				pushCall(callID(it), vocab.ToProvider(responsesToolName(str(it["namespace"]), str(it["name"]))), args)
			case typ == "custom_tool_call":
				input, ok := it["input"].(string)
				if !ok {
					input = responsesOutputText(it["input"])
				}
				pushCall(callID(it), responsesToolName(str(it["namespace"]), str(it["name"])), marshalString(obj{"input": input}))
			case typ == "local_shell_call":
				a := asObj(it["action"])
				args := obj{"command": a["command"]}
				if a["command"] == nil {
					args["command"] = []any{}
				}
				if v, ok := a["working_directory"]; ok && v != nil {
					args["workdir"] = v
				}
				if v, ok := a["timeout_ms"]; ok && v != nil {
					args["timeout_ms"] = v
				}
				pushCall(callID(it), "local_shell", marshalString(args))
			case typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "local_shell_call_output":
				msgs = append(msgs, obj{"role": "tool", "tool_call_id": callID(it), "content": responsesOutputText(it["output"])})
			}
			// reasoning items carry OpenAI's encrypted state, which no other
			// provider reads; hosted-tool items ran at OpenAI. Both are dropped.
		}
	}

	var messages []any
	if len(system) > 0 {
		messages = append(messages, obj{"role": "system", "content": strings.Join(system, "\n\n")})
	}
	for _, m := range healToolPairs(msgs) {
		messages = append(messages, m)
	}
	if len(messages) == 0 || (len(messages) == 1 && len(system) > 0) {
		messages = append(messages, obj{"role": "user", "content": "..."})
	}
	out := obj{"model": in["model"], "messages": messages}
	stream, _ := in["stream"].(bool)
	out["stream"] = stream
	if stream {
		out["stream_options"] = obj{"include_usage": true}
	}

	var tools []any
	walkResponsesTools(responsesToolList(in), func(name string, meta ToolMeta, t obj) {
		fn := obj{"name": name}
		if meta.Kind == "function" {
			fn["name"] = vocab.ToProvider(name)
		}
		switch meta.Kind {
		case "custom":
			fn["description"], fn["parameters"] = customToolDescription(t), customToolParams
		case "local_shell":
			fn["description"], fn["parameters"] = "Run a shell command on the user's machine and return its output.", localShellParams
		default:
			fn["description"] = str(t["description"])
			if strict, ok := t["strict"].(bool); ok {
				fn["strict"] = strict
			}
			params := t["parameters"]
			if params == nil {
				params = obj{"type": "object", "properties": obj{}}
			}
			fn["parameters"] = params
		}
		tools = append(tools, obj{"type": "function", "function": fn})
	})
	if len(tools) > 0 {
		out["tools"] = tools
		if pt, ok := in["parallel_tool_calls"].(bool); ok && !pt {
			out["parallel_tool_calls"] = false
		}
	}
	switch tc := in["tool_choice"].(type) {
	case string:
		out["tool_choice"] = tc
	case obj:
		switch typ := str(tc["type"]); typ {
		case "function", "custom":
			out["tool_choice"] = obj{"type": "function", "function": obj{"name": responsesToolName(str(tc["namespace"]), str(tc["name"]))}}
		case "none", "auto", "required":
			out["tool_choice"] = typ
		case "allowed_tools":
			if m := str(tc["mode"]); m == "required" {
				out["tool_choice"] = m
			} else {
				out["tool_choice"] = "auto"
			}
		default:
			// A hosted tool is not forwarded, so it cannot be forced.
			out["tool_choice"] = "auto"
		}
	}
	if v, ok := in["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := in[k]; ok && v != nil {
			out[k] = v
		}
	}
	if r := asObj(in["reasoning"]); r != nil {
		if e := str(r["effort"]); e != "" && e != "none" {
			out["reasoning_effort"] = e
		}
	}
	switch f := asObj(asObj(in["text"])["format"]); str(f["type"]) {
	case "json_schema":
		if f["schema"] == nil {
			out["response_format"] = obj{"type": "json_object"}
			break
		}
		name := str(f["name"])
		if name == "" {
			name = "response"
		}
		js := obj{"name": name, "schema": f["schema"]}
		if s, ok := f["strict"].(bool); ok {
			js["strict"] = s
		}
		out["response_format"] = obj{"type": "json_schema", "json_schema": js}
	case "json_object":
		out["response_format"] = obj{"type": "json_object"}
	}
	return json.Marshal(out)
}

// responsesContent reads message content as chat parts; allText reports that
// every part is text.
func responsesContent(c any) (parts []any, allText bool) {
	allText = true
	switch v := c.(type) {
	case string:
		if v != "" {
			parts = append(parts, obj{"type": "text", "text": v})
		}
	case []any:
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				if s != "" {
					parts = append(parts, obj{"type": "text", "text": s})
				}
				continue
			}
			p := asObj(raw)
			if str(p["type"]) == "input_image" {
				u := str(p["image_url"])
				if u == "" {
					u = str(asObj(p["image_url"])["url"])
				}
				if u == "" {
					u = str(p["url"])
				}
				if u != "" {
					parts = append(parts, obj{"type": "image_url", "image_url": obj{"url": u}})
					allText = false
				}
			} else if t := str(p["text"]); t != "" {
				parts = append(parts, obj{"type": "text", "text": t})
			}
		}
	}
	return parts, allText
}

// healToolPairs gives every tool call an answer right after its assistant
// turn, and turns a result with no call into user text: a chat provider
// refuses a history where the two do not pair.
func healToolPairs(msgs []obj) []obj {
	var out, deferred []obj
	var pending []string // call ids of the last assistant turn still unanswered
	waiting := func(id string) int {
		for i, p := range pending {
			if p == id {
				return i
			}
		}
		return -1
	}
	orphan := func(m obj) obj {
		label := "[Tool Result]"
		if id := str(m["tool_call_id"]); id != "" {
			label = "[Tool Result (" + id + ")]"
		}
		return obj{"role": "user", "content": label + ": " + str(m["content"])}
	}
	flush := func() {
		for _, id := range pending {
			out = append(out, obj{"role": "tool", "tool_call_id": id, "content": missingToolResult})
		}
		pending = nil
		for _, m := range deferred {
			out = append(out, orphan(m))
		}
		deferred = nil
	}
	for _, m := range msgs {
		if str(m["role"]) == "tool" {
			id := str(m["tool_call_id"])
			if id == "" && len(pending) > 0 {
				id = pending[0]
				m["tool_call_id"] = id
			}
			switch i := waiting(id); {
			case i >= 0:
				pending = append(pending[:i], pending[i+1:]...)
				out = append(out, m)
			case len(pending) > 0:
				deferred = append(deferred, m)
			default:
				out = append(out, orphan(m))
			}
			continue
		}
		flush()
		if calls := list(m["tool_calls"]); len(calls) > 0 {
			for _, raw := range calls {
				c := asObj(raw)
				if str(c["id"]) == "" {
					c["id"] = newID("call_")
				}
				pending = append(pending, str(c["id"]))
			}
		}
		out = append(out, m)
	}
	flush()
	return out
}

// ---- answers: chat -> Responses

func responsesUsage(u obj) obj {
	in, out := num(u["prompt_tokens"]), num(u["completion_tokens"])
	return obj{
		"input_tokens":          in,
		"input_tokens_details":  obj{"cached_tokens": num(asObj(u["prompt_tokens_details"])["cached_tokens"])},
		"output_tokens":         out,
		"output_tokens_details": obj{"reasoning_tokens": num(asObj(u["completion_tokens_details"])["reasoning_tokens"])},
		"total_tokens":          in + out,
	}
}

// lenientArgs parses tool arguments; models often put raw newlines inside JSON
// strings, so a second try escapes control characters.
func lenientArgs(s string) (obj, bool) {
	var m obj
	if json.Unmarshal([]byte(s), &m) == nil {
		return m, true
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 {
			b.WriteString(strings.Trim(marshalString(string(r)), `"`))
			continue
		}
		b.WriteRune(r)
	}
	if json.Unmarshal([]byte(b.String()), &m) == nil {
		return m, true
	}
	return nil, false
}

// responsesToolItem builds the Responses item for one chat tool call, in the
// kind of tool the caller declared.
func responsesToolItem(tools map[string]ToolMeta, itemID, callID, name, args, status string) obj {
	meta, ok := tools[name]
	if !ok {
		meta = ToolMeta{Kind: "function", Name: name}
	}
	done := status == "completed"
	var item obj
	switch meta.Kind {
	case "custom":
		input := ""
		if done {
			if a, ok := lenientArgs(args); ok {
				if s, ok := a["input"].(string); ok {
					input = s
				}
			} else {
				input = args
			}
		}
		if itemID == "" {
			itemID = newID("ctc_")
		}
		item = obj{"id": itemID, "type": "custom_tool_call", "status": status, "call_id": callID, "name": meta.Name, "input": input}
	case "local_shell":
		a, _ := lenientArgs(args)
		command := list(a["command"])
		if command == nil {
			if s := str(a["command"]); s != "" {
				command = []any{"bash", "-lc", s}
			} else {
				command = []any{}
			}
		}
		if itemID == "" {
			itemID = newID("lsh_")
		}
		return obj{"id": itemID, "type": "local_shell_call", "status": status, "call_id": callID,
			"action": obj{"type": "exec", "command": command, "timeout_ms": a["timeout_ms"], "working_directory": a["workdir"], "env": nil, "user": nil}}
	default:
		if !done {
			args = ""
		} else if args == "" {
			args = "{}"
		}
		if itemID == "" {
			itemID = newID("fc_")
		}
		item = obj{"id": itemID, "type": "function_call", "status": status, "name": meta.Name, "arguments": args, "call_id": callID}
	}
	if meta.Namespace != "" {
		item["namespace"] = meta.Namespace
	}
	return item
}

// argsComplete reports whether cut-off arguments still parse; a length stop
// must not hand the caller a call it would run with half its input.
func argsComplete(args string) bool {
	if args == "" {
		return true
	}
	_, ok := lenientArgs(args)
	return ok
}

func chatReasoning(m obj) string {
	if s := str(m["reasoning_content"]); s != "" {
		return s
	}
	return str(m["reasoning"])
}

// OpenAIResponseToResponses converts a whole Chat Completion to a Responses object.
func OpenAIResponseToResponses(body []byte, tools map[string]ToolMeta) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	c := asObj(firstOf(in["choices"]))
	msg := asObj(c["message"])
	incomplete := str(c["finish_reason"]) == "length"
	var output []any
	if t := chatReasoning(msg); t != "" {
		output = append(output, obj{"id": newID("rs_"), "type": "reasoning", "summary": []any{obj{"type": "summary_text", "text": t}}})
	}
	text := contentText(msg["content"])
	calls := list(msg["tool_calls"])
	if text != "" || len(calls) == 0 {
		output = append(output, obj{"id": newID("msg_"), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{obj{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	for _, raw := range calls {
		tc := asObj(raw)
		fn := asObj(tc["function"])
		args := str(fn["arguments"])
		if incomplete && !argsComplete(args) {
			continue
		}
		id := str(tc["id"])
		if id == "" {
			id = newID("call_")
		}
		output = append(output, responsesToolItem(tools, "", id, str(fn["name"]), args, "completed"))
	}
	if output == nil {
		output = []any{}
	}
	status, details := "completed", any(nil)
	if incomplete {
		status, details = "incomplete", obj{"reason": "max_output_tokens"}
	}
	return json.Marshal(obj{
		"id": "resp_" + strings.TrimPrefix(str(in["id"]), "chatcmpl-"), "object": "response",
		"created_at": time.Now().Unix(), "model": in["model"], "status": status,
		"incomplete_details": details, "output": output, "output_text": text,
		"usage": responsesUsage(asObj(in["usage"])),
	})
}

// OpenAIStreamToResponses reads Chat Completions chunks and writes a Responses
// event stream. The Codex CLI builds its history and runs tools from
// response.output_item.done, so every item goes through added, deltas and done.
func OpenAIStreamToResponses(dst Flusher, src io.Reader, tools map[string]ToolMeta) {
	respID, model := newID("resp_"), ""
	created := time.Now().Unix()
	seq, next := 0, 0
	var output []obj
	type textItem struct {
		id    string
		index int
		text  strings.Builder
	}
	type toolItem struct {
		id, callID, name string
		args             strings.Builder
		index            int
		kind             string
		done             bool
	}
	var reasoning, message *textItem
	toolsByIndex := map[int64]*toolItem{}
	var toolOrder []int64
	var usage obj
	finishReason := ""
	started, ended := false, false

	send := func(typ string, ev obj) {
		ev["type"], ev["sequence_number"] = typ, seq
		seq++
		b, _ := json.Marshal(ev)
		io.WriteString(dst, "event: "+typ+"\ndata: "+string(b)+"\n\n")
		dst.Flush()
	}
	snapshot := func(status string, extra obj) obj {
		out := []any{}
		for _, it := range output {
			if it != nil {
				out = append(out, it)
			}
		}
		s := obj{"id": respID, "object": "response", "created_at": created, "model": model, "status": status,
			"output": out, "parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	place := func(index int, item obj) {
		for len(output) <= index {
			output = append(output, nil)
		}
		output[index] = item
	}
	start := func() {
		if !started {
			started = true
			send("response.created", obj{"response": snapshot("in_progress", nil)})
			send("response.in_progress", obj{"response": snapshot("in_progress", nil)})
		}
	}
	closeReasoning := func() {
		if reasoning == nil {
			return
		}
		r := reasoning
		reasoning = nil
		t := r.text.String()
		send("response.reasoning_summary_text.done", obj{"item_id": r.id, "output_index": r.index, "summary_index": 0, "text": t})
		send("response.reasoning_summary_part.done", obj{"item_id": r.id, "output_index": r.index, "summary_index": 0, "part": obj{"type": "summary_text", "text": t}})
		item := obj{"id": r.id, "type": "reasoning", "summary": []any{obj{"type": "summary_text", "text": t}}}
		place(r.index, item)
		send("response.output_item.done", obj{"output_index": r.index, "item": item})
	}
	closeMessage := func() {
		if message == nil {
			return
		}
		m := message
		message = nil
		t := m.text.String()
		part := obj{"type": "output_text", "text": t, "annotations": []any{}}
		send("response.output_text.done", obj{"item_id": m.id, "output_index": m.index, "content_index": 0, "text": t})
		send("response.content_part.done", obj{"item_id": m.id, "output_index": m.index, "content_index": 0, "part": part})
		item := obj{"id": m.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
		place(m.index, item)
		send("response.output_item.done", obj{"output_index": m.index, "item": item})
	}
	closeTools := func() {
		for _, k := range toolOrder {
			t := toolsByIndex[k]
			if t.done {
				continue
			}
			t.done = true
			args := t.args.String()
			item := responsesToolItem(tools, t.id, t.callID, t.name, args, "completed")
			if t.kind == "function" {
				send("response.function_call_arguments.done", obj{"item_id": t.id, "output_index": t.index, "arguments": item["arguments"]})
			}
			place(t.index, item)
			send("response.output_item.done", obj{"output_index": t.index, "item": item})
		}
	}
	think := func(s string) {
		if reasoning == nil {
			closeMessage()
			closeTools()
			reasoning = &textItem{id: newID("rs_"), index: next}
			next++
			send("response.output_item.added", obj{"output_index": reasoning.index, "item": obj{"id": reasoning.id, "type": "reasoning", "summary": []any{}}})
			send("response.reasoning_summary_part.added", obj{"item_id": reasoning.id, "output_index": reasoning.index, "summary_index": 0, "part": obj{"type": "summary_text", "text": ""}})
		}
		reasoning.text.WriteString(s)
		send("response.reasoning_summary_text.delta", obj{"item_id": reasoning.id, "output_index": reasoning.index, "summary_index": 0, "delta": s})
	}
	say := func(s string) {
		if message == nil {
			closeReasoning()
			closeTools()
			message = &textItem{id: newID("msg_"), index: next}
			next++
			send("response.output_item.added", obj{"output_index": message.index, "item": obj{"id": message.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
			send("response.content_part.added", obj{"item_id": message.id, "output_index": message.index, "content_index": 0, "part": obj{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		message.text.WriteString(s)
		send("response.output_text.delta", obj{"item_id": message.id, "output_index": message.index, "content_index": 0, "delta": s})
	}
	call := func(tc obj) {
		key := num(tc["index"])
		fn := asObj(tc["function"])
		t := toolsByIndex[key]
		if t == nil {
			closeReasoning()
			closeMessage()
			name := str(fn["name"])
			kind := "function"
			if m, ok := tools[name]; ok {
				kind = m.Kind
			}
			prefix := map[string]string{"custom": "ctc_", "local_shell": "lsh_"}[kind]
			if prefix == "" {
				prefix = "fc_"
			}
			callID := str(tc["id"])
			if callID == "" {
				callID = newID("call_")
			}
			t = &toolItem{id: newID(prefix), callID: callID, name: name, index: next, kind: kind}
			next++
			toolsByIndex[key] = t
			toolOrder = append(toolOrder, key)
			if kind != "local_shell" {
				send("response.output_item.added", obj{"output_index": t.index, "item": responsesToolItem(tools, t.id, t.callID, t.name, "", "in_progress")})
			}
		}
		if t.name == "" {
			t.name = str(fn["name"])
		}
		if a := str(fn["arguments"]); a != "" && !t.done {
			t.args.WriteString(a)
			if t.kind == "function" {
				send("response.function_call_arguments.delta", obj{"item_id": t.id, "output_index": t.index, "delta": a})
			}
		}
	}
	finish := func() {
		start()
		closeReasoning()
		closeMessage()
		incomplete := finishReason == "length"
		if incomplete {
			for _, t := range toolsByIndex {
				if !argsComplete(t.args.String()) {
					t.done = true
				}
			}
		}
		closeTools()
		status, details := "completed", any(nil)
		if incomplete {
			status, details = "incomplete", obj{"reason": "max_output_tokens"}
		}
		send("response.completed", obj{"response": snapshot(status, obj{"incomplete_details": details, "usage": responsesUsage(usage)})})
		ended = true
	}
	fail := func(msg string) {
		start()
		send("response.failed", obj{"response": snapshot("failed", obj{"error": obj{"code": "server_error", "message": msg}})})
		ended = true
	}

	streamErr := sseEvents(src, func(_, data string) bool {
		if strings.TrimSpace(data) == "[DONE]" {
			finish()
			return false
		}
		ch, err := decode([]byte(data))
		if err != nil {
			return true
		}
		if e := ch["error"]; e != nil {
			msg := str(asObj(e)["message"])
			if msg == "" {
				msg = str(e)
			}
			fail(msg)
			return false
		}
		if model == "" {
			model = str(ch["model"])
		}
		start()
		if u := asObj(ch["usage"]); u != nil {
			usage = u
		}
		c := asObj(firstOf(ch["choices"]))
		if c == nil {
			return true
		}
		d := asObj(c["delta"])
		if s := chatReasoning(d); s != "" {
			think(s)
		}
		if s := str(d["content"]); s != "" {
			say(s)
		}
		for _, tc := range list(d["tool_calls"]) {
			call(asObj(tc))
		}
		if f := str(c["finish_reason"]); f != "" {
			finishReason = f
		}
		return true
	})
	if !ended {
		if streamErr != nil {
			fail("upstream stream ended early: " + streamErr.Error())
			return
		}
		finish()
	}
}
