package translate

import (
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Gemini is Google's generateContent shape, which Antigravity (Cloud Code
// Assist, v1internal) carries inside its own envelope.
const Gemini = "gemini"

// Antigravity is the Gemini shape inside Cloud Code Assist's envelope.
const Antigravity = "antigravity"

// DummyThoughtSignature is the value Google documents for a function call
// replayed without the signature the model gave it (for example a history
// written by another model). The model then skips signature validation.
const DummyThoughtSignature = "skip_thought_signature_validator"

// MaxGeminiOutputTokens is Antigravity's output cap; a larger value is refused.
const MaxGeminiOutputTokens = 64000

// Signatures looks up and records the thought signature of a function call by
// its id, so a later turn can return it with the call.
type Signatures interface {
	Get(callID string) string
	Put(callID, sig string)
}

// OpenAIToGemini converts a Chat Completions request to the inner Gemini
// request (contents, systemInstruction, generationConfig, tools).
func OpenAIToGemini(body []byte, sigs Signatures) ([]byte, error) {
	in, err := decode(body)
	if err != nil {
		return nil, err
	}
	model := str(in["model"])
	out := obj{}
	var system []string
	var contents []obj
	names := map[string]string{} // tool call id -> function name
	add := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			contents[n-1]["parts"] = append(contents[n-1]["parts"].([]any), parts...)
			return
		}
		contents = append(contents, obj{"role": role, "parts": parts})
	}
	for _, raw := range list(in["messages"]) {
		m := asObj(raw)
		switch str(m["role"]) {
		case "system", "developer":
			if t := contentText(m["content"]); t != "" {
				system = append(system, t)
			}
		case "user":
			add("user", geminiParts(m["content"]))
		case "assistant":
			parts := []any{}
			if t := contentText(m["content"]); t != "" {
				parts = append(parts, obj{"text": t})
			}
			for i, tc := range list(m["tool_calls"]) {
				t := asObj(tc)
				fn := asObj(t["function"])
				// The name must match the sanitized declaration name, or Gemini
				// rejects the call: a function named "get weather" is declared as
				// "get_weather", so the call and its response use that form too.
				id, name := str(t["id"]), geminiName(str(fn["name"]))
				names[id] = name
				part := obj{"functionCall": obj{"id": id, "name": name, "args": parseArgs(str(fn["arguments"]))}}
				// The first call of a turn carries the turn's signature.
				if sig := sigs.Get(id); sig != "" {
					part["thoughtSignature"] = sig
				} else if i == 0 {
					part["thoughtSignature"] = DummyThoughtSignature
				}
				parts = append(parts, part)
			}
			add("model", parts)
		case "tool":
			id := str(m["tool_call_id"])
			text := contentText(m["content"])
			var result any = obj{"result": text}
			if v := parseArgs(text); len(asObj(v)) > 0 {
				result = obj{"result": v}
			}
			add("user", []any{obj{"functionResponse": obj{"id": id, "name": names[id], "response": result}}})
		}
	}
	if len(contents) == 0 || contents[0]["role"] != "user" {
		contents = append([]obj{{"role": "user", "parts": []any{obj{"text": "..."}}}}, contents...)
	}
	out["contents"] = contents
	if len(system) > 0 {
		out["systemInstruction"] = obj{"role": "user", "parts": []any{obj{"text": strings.Join(system, "\n\n")}}}
	}

	gen := obj{}
	if v, ok := in["temperature"]; ok {
		gen["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		gen["topP"] = v
	}
	maxOut := num(in["max_tokens"])
	if maxOut == 0 {
		maxOut = num(in["max_completion_tokens"])
	}
	if maxOut > MaxGeminiOutputTokens {
		maxOut = MaxGeminiOutputTokens
	}
	switch s := in["stop"].(type) {
	case string:
		gen["stopSequences"] = []any{s}
	case []any:
		gen["stopSequences"] = s
	}
	if e := str(in["reasoning_effort"]); e != "" && !strings.Contains(strings.ToLower(model), "claude") {
		level := map[string]string{"none": "minimal", "minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high"}[e]
		if level == "" {
			level = "medium"
		}
		gen["thinkingConfig"] = obj{"thinkingLevel": level, "includeThoughts": level != "minimal"}
		// Thinking spends output tokens, so leave room for the answer.
		floor := map[string]int64{"minimal": 4096, "low": 8192, "medium": 16384, "high": MaxGeminiOutputTokens}[level]
		if maxOut < floor {
			maxOut = floor
		}
	}
	if maxOut > 0 {
		gen["maxOutputTokens"] = maxOut
	}
	if len(gen) > 0 {
		out["generationConfig"] = gen
	}

	if tools := list(in["tools"]); len(tools) > 0 {
		var decls []any
		seen := map[string]bool{}
		for _, t := range tools {
			fn := asObj(asObj(t)["function"])
			name := geminiName(str(fn["name"]))
			if fn == nil || name == "" || seen[name] {
				continue
			}
			seen[name] = true
			d := obj{"name": name, "parameters": CleanGeminiSchema(fn["parameters"])}
			if s := str(fn["description"]); s != "" {
				d["description"] = s
			}
			decls = append(decls, d)
		}
		if len(decls) > 0 {
			out["tools"] = []any{obj{"functionDeclarations": decls}}
			mode := "VALIDATED"
			switch tc := in["tool_choice"].(type) {
			case string:
				if tc == "required" {
					mode = "ANY"
				} else if tc == "none" {
					mode = "NONE"
				}
			}
			out["toolConfig"] = obj{"functionCallingConfig": obj{"mode": mode}}
		}
	}
	return json.Marshal(out)
}

var geminiNameRE = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// geminiName makes a function name Gemini accepts: [a-zA-Z_][a-zA-Z0-9_.:-]{0,63}.
func geminiName(n string) string {
	n = geminiNameRE.ReplaceAllString(n, "_")
	if n != "" && !(n[0] == '_' || (n[0] >= 'a' && n[0] <= 'z') || (n[0] >= 'A' && n[0] <= 'Z')) {
		n = "_" + n
	}
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// geminiParts converts OpenAI user content to Gemini parts.
func geminiParts(c any) []any {
	switch v := c.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []any{obj{"text": v}}
	case []any:
		var out []any
		for _, p := range v {
			part := asObj(p)
			switch str(part["type"]) {
			case "text", "input_text":
				if t := str(part["text"]); t != "" {
					out = append(out, obj{"text": t})
				}
			case "image_url":
				u := str(asObj(part["image_url"])["url"])
				if u == "" {
					u = str(part["image_url"])
				}
				if rest, ok := strings.CutPrefix(u, "data:"); ok {
					meta, data, found := strings.Cut(rest, ",")
					mime, _ := strings.CutSuffix(meta, ";base64")
					if found {
						out = append(out, obj{"inlineData": obj{"mimeType": mime, "data": data}})
					}
				} else if u != "" {
					out = append(out, obj{"fileData": obj{"fileUri": u, "mimeType": "image/*"}})
				}
			}
		}
		return out
	}
	return nil
}

// geminiDropKeys are JSON Schema keywords the Gemini schema has no field for;
// one of them anywhere rejects the whole request.
var geminiDropKeys = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`minLength maxLength exclusiveMinimum exclusiveMaximum minItems maxItems
		format multipleOf uniqueItems contains unevaluatedProperties unevaluatedItems contentSchema
		prefixItems additionalItems default examples $schema $defs definitions $ref $comment $id
		deprecated readOnly writeOnly additionalProperties propertyNames patternProperties enumDescriptions
		not dependencies dependentSchemas dependentRequired title optional if then else
		contentMediaType contentEncoding strict encrypted cache_control example`) {
		geminiDropKeys[k] = true
	}
}

// CleanGeminiSchema rewrites a JSON Schema into the subset Gemini accepts:
// const becomes enum, anyOf/oneOf keep their best branch, allOf is merged,
// a type list keeps its first non-null type, and unsupported keywords go.
func CleanGeminiSchema(v any) any {
	s := asObj(v)
	if s == nil {
		return obj{"type": "object", "properties": obj{"reason": obj{"type": "string"}}, "required": []any{"reason"}}
	}
	return cleanSchema(s)
}

func cleanSchema(s obj) obj {
	out := obj{}
	// Fold combinators into the schema itself first.
	for _, k := range []string{"anyOf", "oneOf"} {
		if branches := list(s[k]); len(branches) > 0 {
			best := pickBranch(branches)
			for bk, bv := range best {
				if _, has := s[bk]; !has {
					s[bk] = bv
				}
			}
		}
	}
	for _, b := range list(s["allOf"]) {
		for bk, bv := range asObj(b) {
			if bk == "properties" {
				props := asObj(s["properties"])
				if props == nil {
					props = obj{}
				}
				for pk, pv := range asObj(bv) {
					props[pk] = pv
				}
				s["properties"] = props
			} else if _, has := s[bk]; !has {
				s[bk] = bv
			}
		}
	}
	for k, val := range s {
		switch {
		case k == "anyOf" || k == "oneOf" || k == "allOf" || geminiDropKeys[k] || strings.HasPrefix(k, "x-"):
		case k == "const":
			out["enum"] = []any{stringify(val)}
		case k == "enum":
			var e []any
			for _, x := range list(val) {
				if x != nil {
					e = append(e, stringify(x))
				}
			}
			// An empty enum must not become "enum": null, which Gemini rejects.
			if len(e) > 0 {
				out["enum"] = e
			}
		case k == "type":
			if l := list(val); l != nil {
				for _, t := range l {
					if t != "null" {
						out["type"] = t
						break
					}
				}
			} else {
				out["type"] = val
			}
		case k == "properties":
			props := obj{}
			for pk, pv := range asObj(val) {
				if ps := asObj(pv); ps != nil {
					props[pk] = cleanSchema(ps)
				}
			}
			out["properties"] = props
		case k == "items":
			if l := list(val); l != nil {
				if len(l) > 0 {
					out["items"] = cleanSchema(asObj(l[0]))
				}
			} else if is := asObj(val); is != nil {
				out["items"] = cleanSchema(is)
			}
		default:
			out[k] = val
		}
	}
	if out["type"] == nil && out["properties"] != nil {
		out["type"] = "object"
	}
	if out["enum"] != nil {
		out["type"] = "string"
	}
	if out["type"] == "array" && out["items"] == nil {
		out["items"] = obj{"type": "string"}
	}
	if out["type"] == "object" {
		props := asObj(out["properties"])
		if len(props) == 0 {
			out["properties"] = obj{"reason": obj{"type": "string"}}
			out["required"] = []any{"reason"}
		} else if req := list(out["required"]); req != nil {
			var keep []any
			for _, r := range req {
				if _, ok := props[str(r)]; ok {
					keep = append(keep, r)
				}
			}
			if keep == nil {
				delete(out, "required")
			} else {
				out["required"] = keep
			}
		}
	}
	return out
}

// pickBranch prefers an object branch, then an array, then any non-null one.
func pickBranch(branches []any) obj {
	var best obj
	rank := func(b obj) int {
		switch str(b["type"]) {
		case "object":
			return 3
		case "array":
			return 2
		case "null":
			return 0
		}
		return 1
	}
	for _, b := range branches {
		if bo := asObj(b); bo != nil && (best == nil || rank(bo) > rank(best)) {
			best = bo
		}
	}
	return best
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

var geminiFinish = map[string]string{
	"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter", "RECITATION": "content_filter",
	"BLOCKLIST": "content_filter", "PROHIBITED_CONTENT": "content_filter", "SPII": "content_filter",
}

// GeminiStreamToOpenAI reads a Gemini (or Antigravity-wrapped) event stream
// and writes Chat Completions chunks. Thought signatures of function calls are
// recorded so the next turn can send them back.
func GeminiStreamToOpenAI(dst Flusher, src io.Reader, sigs Signatures) {
	id, model := newID("chatcmpl-"), ""
	created := time.Now().Unix()
	started := false
	calls := 0
	finish := ""
	pendingSig := ""
	var usage obj
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
	streamErr := sseEvents(src, func(_, data string) bool {
		ev, err := decode([]byte(data))
		if err != nil {
			return true
		}
		if e := asObj(ev["error"]); e != nil {
			b, _ := json.Marshal(obj{"error": obj{"message": str(e["message"]), "type": str(e["status"])}})
			io.WriteString(dst, "data: "+string(b)+"\n\n")
			dst.Flush()
			return false
		}
		r := asObj(ev["response"])
		if r == nil {
			r = ev
		}
		if !started {
			started = true
			if s := str(r["responseId"]); s != "" {
				id = "chatcmpl-" + s
			}
			model = str(r["modelVersion"])
			emit(obj{"role": "assistant", "content": ""}, nil, nil)
		}
		if u := asObj(r["usageMetadata"]); u != nil {
			usage = u
		}
		cand := asObj(firstOf(r["candidates"]))
		for _, p := range list(asObj(cand["content"])["parts"]) {
			part := asObj(p)
			sig := str(part["thoughtSignature"])
			switch {
			case part["functionCall"] != nil:
				fc := asObj(part["functionCall"])
				cid := str(fc["id"])
				if cid == "" {
					cid = "call_" + newID("")
				}
				if sig == "" {
					sig = pendingSig
				}
				if sig != "" && sigs != nil {
					sigs.Put(cid, sig)
				}
				pendingSig = ""
				args, _ := json.Marshal(fc["args"])
				emit(obj{"tool_calls": []any{obj{"index": calls, "id": cid, "type": "function",
					"function": obj{"name": fc["name"], "arguments": string(args)}}}}, nil, nil)
				calls++
			case part["thought"] == true:
				if t := str(part["text"]); t != "" {
					emit(obj{"reasoning_content": t}, nil, nil)
				}
				if sig != "" {
					pendingSig = sig
				}
			case part["text"] != nil:
				if t := str(part["text"]); t != "" {
					emit(obj{"content": t}, nil, nil)
				}
				if sig != "" {
					pendingSig = sig
				}
			default:
				if sig != "" {
					pendingSig = sig
				}
			}
		}
		if f := str(cand["finishReason"]); f != "" {
			finish = geminiFinish[f]
			if finish == "" {
				finish = "stop"
			}
		}
		return true
	})
	if streamErr != nil {
		// A truncated stream must not close as a clean answer.
		b, _ := json.Marshal(obj{"error": obj{"message": "upstream stream ended early: " + streamErr.Error(), "type": "api_error"}})
		io.WriteString(dst, "data: "+string(b)+"\n\n")
		dst.Flush()
		return
	}
	if !started {
		emit(obj{"role": "assistant", "content": ""}, nil, nil)
	}
	if finish == "" || (finish == "stop" && calls > 0) {
		if calls > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	var extra obj
	if usage != nil {
		in := num(usage["promptTokenCount"])
		thoughts := num(usage["thoughtsTokenCount"])
		out := num(usage["candidatesTokenCount"]) + thoughts
		us := obj{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
		if c := num(usage["cachedContentTokenCount"]); c > 0 {
			us["prompt_tokens_details"] = obj{"cached_tokens": c}
		}
		if thoughts > 0 {
			us["completion_tokens_details"] = obj{"reasoning_tokens": thoughts}
		}
		extra = obj{"usage": us}
	}
	emit(obj{}, finish, extra)
	io.WriteString(dst, "data: [DONE]\n\n")
	dst.Flush()
}
