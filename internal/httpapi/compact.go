package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/louisphamdev/intact/internal/store"
)

// Rate-limit compaction.
//
// A provider that answers 429 has run out of room for this request right now. For a
// short conversation that is a rate limit and nothing else: wait, or go elsewhere.
// For a long one it is also a size problem, and the account that answers next meets
// the same conversation against the same wall.
//
// The limits in this area are counted in tokens per minute, so a shorter request is
// more likely to fit, and the account that answers next holds no cache for this
// prefix either: it never saw the first answer, so the long history costs it full
// price and buys nothing. Shortening the history before the retry is therefore not a
// guess about limits. It is the one change that makes the retry both cheaper and more
// likely to be accepted.
//
// It is lossy, so it runs under two conditions and no others. The provider must be in
// round-robin rotation with another account ready to take the session: a provider that
// pins one account has nowhere to send a shortened conversation, so shortening it
// would only have thrown history away. And it must be a real 429 -- not a model
// declining to answer, and not a shape intact should have refused earlier.
//
// Not a model-written summary, deliberately. A summary would keep more of the
// history, and it would cost a request that can itself be rate limited, add its own
// latency, and fail in exactly the situation where the retry matters. Truncation is
// deterministic and cannot fail. If a version of this ever wants a summary, it belongs
// on the account that just answered, before the history is thrown away, under the same
// byte budget.

// compactPolicy is how much of a conversation survives. The defaults keep the shape of
// an agent session readable: what was asked at the start, what the assistant said while
// working, and every tool call of the recent turns, which is what the next turn's
// reasoning leans on.
type compactPolicy struct {
	Enabled        *bool `json:"enabled,omitempty"` // nil means on
	MarkerOn       *bool `json:"marker,omitempty"`  // nil means on: say what was dropped
	KeepRecent     int   `json:"keepRecent,omitempty"`
	HeadBytes      int   `json:"headBytes,omitempty"`
	UserBytes      int   `json:"userBytes,omitempty"`
	AssistantBytes int   `json:"assistantBytes,omitempty"`
	MinBytes       int   `json:"minBytes,omitempty"`
}

const rateLimitCompactKey = "cache:rate-limit-compact"

func (p compactPolicy) on() bool { return p.Enabled == nil || *p.Enabled }

func (p compactPolicy) withDefaults() compactPolicy {
	if p.KeepRecent <= 0 || p.KeepRecent > 100000 {
		p.KeepRecent = 6
	}
	if p.HeadBytes <= 0 {
		p.HeadBytes = 6000
	}
	if p.UserBytes <= 0 {
		p.UserBytes = 3000
	}
	if p.AssistantBytes <= 0 {
		p.AssistantBytes = 1500
	}
	if p.MinBytes <= 0 {
		p.MinBytes = 24000
	}
	return p
}

// rateLimitCompact reads the policy. A setting that does not parse refuses lossy compaction
// to preserve the original body safely. An absent setting leaves the defaults in place.
func (a *api) rateLimitCompact() compactPolicy {
	p := compactPolicy{}
	v, err := a.store.GetSetting(rateLimitCompactKey)
	if err != nil || v == "" {
		return p.withDefaults()
	}
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		log.Printf("cache: %s: %v; refusing lossy compaction", rateLimitCompactKey, err)
		disabled := false
		return compactPolicy{Enabled: &disabled}
	}
	return p.withDefaults()
}

// compactStat is what one compaction did, for the log line and the reply header.
type compactStat struct {
	before, after   int
	keptHead        int
	keptMiddle      int
	droppedMiddle   int
	droppedToolOnly int
}

func (s compactStat) saved() int { return s.before - s.after }

// historyItem is one message of the conversation, read far enough to know its role,
// its words, and whether it carries a tool result.
type historyItem struct {
	raw       json.RawMessage
	role      string
	text      string
	hasResult bool
}

// compactForRateLimit answers the body to retry a rate-limited request with, or false
// when the retry should carry the conversation as it is.
//
// rest is the accounts still to be tried. It must hold at least one that is not a
// standby: a standby is the last resort for a provider with nothing else, and handing it
// a shortened conversation is not worth the loss.
func (a *api) compactForRateLimit(prov string, rest []store.Connection, body []byte) ([]byte, compactStat, bool) {
	if len(rest) == 0 {
		return nil, compactStat{}, false
	}
	if m, ok := bodyModel(body); !ok || m == "" {
		return nil, compactStat{}, false
	}
	p := a.rateLimitCompact()
	if !p.on() || len(body) < p.MinBytes {
		return nil, compactStat{}, false
	}
	// Round-robin means a session is spread over accounts and any of them can take
	// the next request. A provider ordered otherwise keeps one account per session,
	// and there is no second account to receive a shortened conversation.
	if a.rotation(prov).Mode != RotateRoundRobin {
		return nil, compactStat{}, false
	}
	ready := false
	for _, c := range rest {
		if !c.Standby {
			ready = true
			break
		}
	}
	if !ready {
		return nil, compactStat{}, false
	}
	return compactConversation(body, p,
		fmt.Sprintf("%s answered 429 for it", prov))
}

// compactConversation shortens the history a request carries. It answers false, and
// changes nothing, whenever the conversation is not worth shortening: too few messages,
// too few bytes, or nothing between the opening request and the recent turns to shed.
func compactConversation(body []byte, p compactPolicy, reason string) ([]byte, compactStat, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil, compactStat{}, false
	}
	field, raw, ok := historyField(top)
	if !ok {
		return nil, compactStat{}, false
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, compactStat{}, false
	}
	if p.KeepRecent < 2 || p.KeepRecent >= len(items)-2 || len(body) < p.MinBytes {
		return nil, compactStat{}, false
	}

	parsed := make([]historyItem, len(items))
	for i, it := range items {
		role, text, hasResult := readItem(it)
		parsed[i] = historyItem{raw: it, role: role, text: text, hasResult: hasResult}
	}

	// The opening request is what the whole conversation is for, so it stays even when
	// it is nearly all that is left after the middle goes.
	head := -1
	for i, it := range parsed {
		if it.role != "system" && it.role != "developer" {
			head = i
			break
		}
	}
	if head < 0 {
		return nil, compactStat{}, false
	}

	// The tail is kept as the client wrote it, tool calls and all, because the next
	// turn's reasoning leans on the last few tool results verbatim. It starts on a
	// message that carries no result: a result whose call was cut away is a request
	// the provider refuses.
	tail := len(parsed) - p.KeepRecent
	if tail < 0 || tail >= len(parsed) {
		return nil, compactStat{}, false
	}
	for tail < len(parsed) && parsed[tail].hasResult {
		tail++
	}
	if tail <= head+1 || tail > len(parsed) {
		return nil, compactStat{}, false
	}

	st := compactStat{before: len(body)}
	kept := make([]json.RawMessage, 0, len(parsed))
	// Whatever came before the first request: system prompts, developer instructions,
	// a Code Assist preamble. Unchanged, because it is not history.
	for i := range parsed[:head] {
		kept = append(kept, parsed[i].raw)
	}
	kept = append(kept, shrinkTo(&parsed[head], p.HeadBytes, field))
	st.keptHead = 1

	if p.MarkerOn == nil || *p.MarkerOn {
		kept = append(kept, markerMessage(reason, &st, p.KeepRecent, field))
	}
	for i := head + 1; i < tail; i++ {
		it := &parsed[i]
		limit := p.AssistantBytes
		if it.role == "user" {
			limit = p.UserBytes
		}
		if text := trimTo(it.text, limit); text != "" {
			kept = append(kept, rebuildItem(field, it.role, text))
			st.keptMiddle++
			continue
		}
		// No words left means the message held a tool call, a tool result or a
		// reasoning block. All three are most of the weight of a session and none is
		// what a summary of it would keep, so they go.
		st.droppedMiddle++
		if it.hasResult || it.role == "tool" {
			st.droppedToolOnly++
		}
	}
	for i := tail; i < len(parsed); i++ {
		kept = append(kept, parsed[i].raw)
	}

	out, err := putHistory(body, field, kept)
	if err != nil {
		return nil, compactStat{}, false
	}
	st.after = len(out)
	if st.after >= st.before {
		return nil, compactStat{}, false
	}
	return out, st, true
}

// historyField names where the request keeps its conversation. Chat Completions and
// Messages both use messages, Responses uses input, and a Code Assist envelope carries
// Gemini contents inside request.
func historyField(top map[string]json.RawMessage) (string, json.RawMessage, bool) {
	for _, f := range []string{"messages", "input"} {
		if raw, ok := top[f]; ok && len(raw) > 2 {
			return f, raw, true
		}
	}
	if req, ok := top["request"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(req, &inner) == nil {
			if raw, ok := inner["contents"]; ok && len(raw) > 2 {
				return "request.contents", raw, true
			}
		}
	}
	return "", nil, false
}

// putHistory writes the history back into the body it came from, leaving every other
// field as it was. The messages are the only thing this changes: tools, model, thinking
// and the client's own additions are none of this function's business.
func putHistory(body []byte, field string, items []json.RawMessage) ([]byte, error) {
	arr, err := marshalPlain(items)
	if err != nil {
		return nil, err
	}
	if field == "request.contents" {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			return nil, err
		}
		// The contents live inside the Code Assist envelope, so it is the envelope's
		// own map that gets the new array -- the body around it is untouched.
		var env map[string]json.RawMessage
		if err := json.Unmarshal(top["request"], &env); err != nil {
			return nil, err
		}
		env["contents"] = arr
		inner, err := marshalPlain(env)
		if err != nil {
			return nil, err
		}
		top["request"] = inner
		return putTop(top)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	top[field] = arr
	return putTop(top)
}

// marshalPlain encodes without Go's HTML escaping, so a conversation holding a < or an
// & comes out of compaction as the same JSON it went in as. json.Marshal cannot be used
// for it: it escapes < and & inside the raw messages it re-encodes, which rewrites the
// client's own text.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// putTop re-encodes the request around the shortened history.
func putTop(top map[string]json.RawMessage) ([]byte, error) { return marshalPlain(top) }

type rawItem struct {
	Role      string          `json:"role"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Parts     json.RawMessage `json:"parts"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

// readItem returns an item's role, the words in it, and whether it carries a tool
// result. The four request shapes store the same conversation four ways, and an item
// none of them explain yields no words -- which drops it, rather than risk sending a
// tool call without the result the provider requires.
func readItem(raw json.RawMessage) (role, text string, hasResult bool) {
	var it rawItem
	if json.Unmarshal(raw, &it) != nil {
		return "", "", false
	}
	role = it.Role
	// Chat Completions puts a tool result in a message of its own.
	if role == "tool" {
		return role, "", true
	}
	// A Responses item that is a call, a result or reasoning has no words of its own.
	switch it.Type {
	case "function_call", "custom_tool_call", "local_shell_call":
		return role, "", false
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return role, "", true
	case "reasoning":
		return role, "", false
	}
	switch {
	case len(it.Parts) > 0:
		text, hasResult = partsText(it.Parts)
	case len(it.Content) > 0:
		text, hasResult = contentText(it.Content)
	}
	return role, text, hasResult
}

// contentText reads a string or a list of blocks, keeping the words and noting a tool
// result. Blocks that are not words -- images, documents, thinking -- are left out of
// the middle on purpose: they are much of the weight and none of the thread.
func contentText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	hasResult := false
	for _, bl := range blocks {
		switch bl.Type {
		case "text", "input_text", "output_text", "summary_text", "refusal":
			b.WriteString(bl.Text)
		case "tool_result":
			hasResult = true
		}
	}
	return b.String(), hasResult
}

// partsText reads Gemini contents, where text, a function call and a function response
// are three kinds of part.
func partsText(raw json.RawMessage) (string, bool) {
	var parts []struct {
		Text             string          `json:"text"`
		FunctionCall     json.RawMessage `json:"functionCall"`
		FunctionResponse json.RawMessage `json:"functionResponse"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var b strings.Builder
	hasResult := false
	for _, p := range parts {
		switch {
		case p.Text != "":
			b.WriteString(p.Text)
		case len(p.FunctionResponse) > 0:
			hasResult = true
		}
	}
	return b.String(), hasResult
}

// shrinkTo keeps a message whole when it is short enough, and rebuilds it as its words
// when it is not. An opening request that was mostly pasted text loses the tail of the
// paste rather than its meaning.
func shrinkTo(it *historyItem, max int, field string) json.RawMessage {
	if len(it.text) <= max {
		return it.raw
	}
	return rebuildItem(field, it.role, trimTo(it.text, max))
}

// rebuildItem writes a message according to the specific protocol of field.
func rebuildItem(field, role, text string) json.RawMessage {
	switch field {
	case "input":
		typeStr := "output_text"
		if role == "user" {
			typeStr = "input_text"
		}
		item := map[string]any{
			"type": "message",
			"role": role,
			"content": []map[string]string{
				{"type": typeStr, "text": text},
			},
		}
		b, _ := marshalPlain(item)
		return b
	case "contents", "request.contents":
		geminiRole := role
		if role == "assistant" {
			geminiRole = "model"
		}
		item := map[string]any{
			"role": geminiRole,
			"parts": []map[string]string{
				{"text": text},
			},
		}
		b, _ := marshalPlain(item)
		return b
	default:
		b, _ := marshalPlain(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{role, text})
		return b
	}
}

// trimTo shortens text to a byte budget on a rune boundary, and says what went, so the
// model can tell a truncated quote from a whole one.
func trimTo(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[... %d characters cut here ...]", utf8.RuneCountInString(text[cut:]))
}

// markerMessage is the one message compaction adds. It says what went and why, because
// a model handed a conversation with a hole in it will otherwise read the hole as
// something it forgot rather than something that was cut.
func markerMessage(reason string, st *compactStat, keepRecent int, field string) json.RawMessage {
	what := fmt.Sprintf("%d earlier messages were kept as their text alone", st.keptMiddle)
	if st.droppedMiddle > 0 {
		what += fmt.Sprintf(", and %d were dropped whole because they held only tool calls and their results", st.droppedMiddle)
	}
	userRole := "user"
	msg := fmt.Sprintf(
		"[intact] The history between the first request and the recent turns was shortened before this "+
			"request was sent, because %s. The next account holds no cache for this conversation, so the "+
			"extra length would have been paid for in full and returned nothing. %s. The opening request and "+
			"the last %d messages are unchanged.",
		reason, what, keepRecent)
	return rebuildItem(field, userRole, msg)
}
