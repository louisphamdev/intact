package httpapi

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ModelInfo is what a provider's model list says about a model's thinking.
// Thinking is nil when the provider says nothing about it.
type ModelInfo struct {
	Thinking *bool `json:"thinking,omitempty"`
	// Always: the model cannot answer without thinking.
	Always bool `json:"always,omitempty"`
	// Efforts are the levels the request may ask for, as the provider names them.
	Efforts []string `json:"efforts,omitempty"`
	Default string   `json:"default,omitempty"`
	// Paid: the provider serves it only on a paid plan.
	Paid bool `json:"paid,omitempty"`
	// Token limits: the whole window, the prompt, the answer. 0 is unknown.
	Context int64 `json:"context,omitempty"`
	Input   int64 `json:"input,omitempty"`
	Output  int64 `json:"output,omitempty"`
}

// modelInfos reads thinking support from a model list answer. Each provider
// says it its own way:
//
//	Anthropic   capabilities.thinking.supported, capabilities.effort.<level>.supported
//	Codex       supported_reasoning_levels[].effort, default_reasoning_level
//	Copilot     capabilities.supports.reasoning_effort, adaptive_thinking, max_thinking_budget
//	OpenRouter  supported_parameters has "reasoning" / "reasoning_effort", reasoning.mandatory
//	Groq        supported_features has "reasoning"
//	Antigravity models.<id>.supportsThinking
//	Cloudflare  properties: reasoning, reasoning_effort{supported_efforts, default_effort, mandatory}
//
// When a provider says it of any model, a model it says nothing of is marked
// as not thinking; a provider that never says leaves every model unknown.
func modelInfos(raw []byte) map[string]ModelInfo {
	var d struct {
		Data   []map[string]any `json:"data"`
		Result []map[string]any `json:"result"`
		Models json.RawMessage  `json:"models"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	items := map[string]map[string]any{}
	for _, m := range d.Data {
		if id := firstNonEmpty(str(m["id"]), str(m["slug"]), str(m["name"])); id != "" {
			items[id] = m
		}
	}
	// Cloudflare: id is a UUID, the model is its name.
	for _, m := range d.Result {
		if id := str(m["name"]); id != "" {
			items[id] = m
		}
	}
	var list []map[string]any
	var byKey map[string]map[string]any
	if json.Unmarshal(d.Models, &list) == nil {
		for _, m := range list {
			if id := firstNonEmpty(str(m["slug"]), str(m["id"]), str(m["name"])); id != "" {
				items[id] = m
			}
		}
	} else if json.Unmarshal(d.Models, &byKey) == nil {
		for id, m := range byKey {
			items[id] = m
		}
	}
	out := map[string]ModelInfo{}
	said := map[string]bool{}
	for id, m := range items {
		info, ok := readInfo(m)
		info.Context, info.Input, info.Output = readLimits(m)
		if ok {
			said[id] = true
		}
		if ok || info.Context+info.Input+info.Output > 0 {
			out[id] = info
		}
	}
	// Limits alone say nothing of thinking, so thinking stays unknown.
	if len(said) == 0 {
		if len(out) == 0 {
			return nil
		}
		return out
	}
	no := false
	for id := range items {
		if !said[id] {
			info := out[id]
			info.Thinking = &no
			out[id] = info
		}
	}
	return out
}

// readLimits reads a model's token limits under each provider's own field
// names; the first field found wins. The names are listed in docs/models.md.
func readLimits(m map[string]any) (context, input, output int64) {
	pick := func(dst *int64, v any) {
		if n := tokenCount(v); n > 0 && *dst == 0 {
			*dst = n
		}
	}
	caps, _ := m["capabilities"].(map[string]any)
	if l, ok := caps["limits"].(map[string]any); ok {
		pick(&context, l["max_context_window_tokens"])
		pick(&input, l["max_prompt_tokens"])
		pick(&output, l["max_output_tokens"])
	}
	if tp, ok := m["top_provider"].(map[string]any); ok {
		pick(&context, tp["context_length"])
		pick(&output, tp["max_completion_tokens"])
	}
	for _, k := range []string{"max_context_window", "context_length", "context_window", "maxTokens"} {
		pick(&context, m[k])
	}
	for _, k := range []string{"max_input_tokens", "inputTokenLimit"} {
		pick(&input, m[k])
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "maxOutputTokens", "outputTokenLimit"} {
		pick(&output, m[k])
	}
	if props, ok := m["properties"].([]any); ok {
		for _, p := range props {
			if o, _ := p.(map[string]any); str(o["property_id"]) == "context_window" {
				pick(&context, o["value"])
			}
		}
	}
	return context, input, output
}

// tokenCount reads a count given as a number or as a numeric string.
func tokenCount(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func readInfo(m map[string]any) (ModelInfo, bool) {
	var info ModelInfo
	said := false
	yes := func(v bool) {
		said = true
		if info.Thinking == nil || v {
			info.Thinking = &v
		}
	}
	addEffort := func(e string) {
		if e != "" && !contains(info.Efforts, e) {
			info.Efforts = append(info.Efforts, e)
		}
	}
	caps, _ := m["capabilities"].(map[string]any)
	// Anthropic.
	if t, ok := caps["thinking"].(map[string]any); ok {
		yes(t["supported"] == true)
	}
	if e, ok := caps["effort"].(map[string]any); ok && e["supported"] == true {
		for _, l := range effortOrder {
			if v, ok := e[l].(map[string]any); ok && v["supported"] == true {
				addEffort(l)
			}
		}
	}
	// Copilot.
	if s, ok := caps["supports"].(map[string]any); ok {
		for _, e := range strs(s["reasoning_effort"]) {
			addEffort(e)
		}
		if len(info.Efforts) > 0 || s["adaptive_thinking"] == true || num(s["max_thinking_budget"]) > 0 {
			yes(true)
		} else {
			yes(false)
		}
	}
	// Codex.
	if levels, ok := m["supported_reasoning_levels"].([]any); ok {
		for _, l := range levels {
			if o, ok := l.(map[string]any); ok {
				addEffort(str(o["effort"]))
			}
		}
		yes(len(levels) > 0)
		info.Default = str(m["default_reasoning_level"])
	}
	// OpenRouter.
	if params, ok := m["supported_parameters"].([]any); ok {
		p := strs(params)
		yes(contains(p, "reasoning") || contains(p, "include_reasoning"))
		if contains(p, "reasoning_effort") && len(info.Efforts) == 0 {
			info.Efforts = []string{"low", "medium", "high"}
		}
		if r, ok := m["reasoning"].(map[string]any); ok && r["mandatory"] == true {
			info.Always = true
		}
	}
	// Groq.
	if f, ok := m["supported_features"].([]any); ok {
		yes(contains(strs(f), "reasoning"))
	}
	// Antigravity.
	if v, ok := m["supportsThinking"].(bool); ok {
		yes(v)
	}
	// Cloudflare: properties [{property_id, value}].
	if props, ok := m["properties"].([]any); ok {
		thinks := false
		for _, p := range props {
			o, _ := p.(map[string]any)
			switch str(o["property_id"]) {
			case "reasoning":
				thinks = thinks || str(o["value"]) == "true"
			case "reasoning_effort":
				thinks = true
				if v, ok := o["value"].(map[string]any); ok {
					for _, e := range strs(v["supported_efforts"]) {
						addEffort(e)
					}
					info.Default = str(v["default_effort"])
					info.Always = v["mandatory"] == true
				}
			case "require_workers_paid":
				info.Paid = str(o["value"]) == "true"
			}
		}
		yes(thinks)
	}
	return info, said
}

var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// infoFor merges what a list says of a model's variants into the model.
func foldInfos(infos map[string]ModelInfo, groups map[string]variantSet) map[string]ModelInfo {
	if infos == nil {
		return nil
	}
	for base, g := range groups {
		var merged ModelInfo
		for _, l := range g.Variants() {
			i, ok := infos[g.ids[l]]
			if ok && i.Thinking != nil && (merged.Thinking == nil || *i.Thinking) {
				merged.Thinking = i.Thinking
			}
			merged.Context = max(merged.Context, i.Context)
			merged.Input = max(merged.Input, i.Input)
			merged.Output = max(merged.Output, i.Output)
			delete(infos, g.ids[l])
		}
		merged.Efforts = g.Variants()
		merged.Default = g.def
		infos[base] = merged
	}
	return infos
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) float64 { f, _ := v.(float64); return f }

func strs(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, strings.ToLower(s))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

func rank(e string) int {
	for i, x := range effortOrder {
		if x == e {
			return i
		}
	}
	return len(effortOrder)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
