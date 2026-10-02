// Package usage extracts token counts from a provider response body. It reads
// the response only to record a number; it never changes the bytes that reach
// the caller. It understands both field vocabularies (OpenAI
// prompt/completion, Anthropic input/output) and both shapes (a plain JSON body
// and a Server-Sent-Events stream).
package usage

import (
	"bytes"
	"encoding/json"
)

// Counts is the token usage read from one response.
type Counts struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	// CachedTokens are the input tokens read from the provider's cache, a part
	// of InputTokens.
	CachedTokens int64
	Found        bool
}

// envelope covers every place the providers put usage. A streamed Anthropic
// chunk nests model and usage under "message", a Responses event and a Code
// Assist chunk under "response"; every other shape puts them at the top level.
type envelope struct {
	Model         string       `json:"model"`
	Usage         *usage       `json:"usage"`
	ModelVersion  string       `json:"modelVersion"`
	UsageMetadata *geminiUsage `json:"usageMetadata"`
	Message       *nested      `json:"message"`
	Response      *nested      `json:"response"`
}

type nested struct {
	Model         string       `json:"model"`
	Usage         *usage       `json:"usage"`
	ModelVersion  string       `json:"modelVersion"`
	UsageMetadata *geminiUsage `json:"usageMetadata"`
}

// geminiUsage is Gemini's vocabulary. Thought tokens are billed as output, and
// the cached tokens are a part of the prompt.
type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

func (g *geminiUsage) asUsage() *usage {
	if g == nil {
		return nil
	}
	return &usage{PromptTokens: g.PromptTokenCount, CompletionTokens: g.CandidatesTokenCount + g.ThoughtsTokenCount,
		PromptTokensDetails: &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{g.CachedContentTokenCount}}
}

type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	// Anthropic reports cached prompt tokens apart from input_tokens. They are
	// still prompt tokens the account paid for, so they count as input.
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	// OpenAI Chat and Responses report the cached part inside the input.
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// Parse extracts token usage from a full response body. For a stream it merges
// every data chunk: input and output tokens keep the largest value seen, which
// is correct for OpenAI (the final chunk holds the totals) and for Anthropic
// (message_start sets input, message_delta raises the cumulative output).
func Parse(body []byte) Counts {
	var c Counts
	for _, obj := range jsonObjects(body) {
		var e envelope
		if err := json.Unmarshal(obj, &e); err != nil {
			continue
		}
		model, u := firstNonEmpty(e.Model, e.ModelVersion), e.Usage
		if u == nil {
			u = e.UsageMetadata.asUsage()
		}
		for _, n := range []*nested{e.Message, e.Response} {
			if n == nil {
				continue
			}
			if m := firstNonEmpty(n.Model, n.ModelVersion); m != "" {
				model = m
			}
			if n.Usage != nil {
				u = n.Usage
			} else if n.UsageMetadata != nil {
				u = n.UsageMetadata.asUsage()
			}
		}
		if model != "" && c.Model == "" {
			c.Model = model
		}
		if u == nil {
			continue
		}
		in := u.PromptTokens + u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		out := u.CompletionTokens + u.OutputTokens
		if in > c.InputTokens {
			c.InputTokens = in
		}
		if out > c.OutputTokens {
			c.OutputTokens = out
		}
		cached := u.CacheReadInputTokens
		if d := u.PromptTokensDetails; d != nil {
			cached += d.CachedTokens
		}
		if d := u.InputTokensDetails; d != nil {
			cached += d.CachedTokens
		}
		if cached > c.CachedTokens {
			c.CachedTokens = cached
		}
		if in > 0 || out > 0 {
			c.Found = true
		}
	}
	return c
}

// jsonObjects returns each JSON object in the body. A Server-Sent-Events stream
// carries one object per "data:" line; a plain response is a single object.
func jsonObjects(body []byte) [][]byte {
	// A single JSON object starts with '{'. Deciding SSE by the substring
	// "data:" alone is wrong: a plain response can carry "data:" in its text
	// (a data URI, a code sample), and that must not turn it into a stream.
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return [][]byte{body}
	}
	if !bytes.Contains(body, []byte("data:")) {
		return [][]byte{body}
	}
	var out [][]byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		out = append(out, payload)
	}
	// If nothing matched the SSE shape, fall back to treating the whole body as
	// one object rather than dropping it.
	if len(out) == 0 {
		return [][]byte{body}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
