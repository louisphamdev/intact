package httpapi

import (
	"io"
	"net/http"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

// shapePath is the endpoint of each request shape under a provider's base URL.
var shapePath = map[string]string{
	translate.OpenAI:    "chat/completions",
	translate.Anthropic: "messages",
}

// clientShape names the shape of a caller's request from its path, or "" for a
// path intact forwards without translating (embeddings, responses, …).
func clientShape(path string) string {
	switch strings.Trim(path, "/") {
	case "chat/completions":
		return translate.OpenAI
	case "messages":
		return translate.Anthropic
	}
	return ""
}

// shapeOf names the request shape a provider speaks.
func shapeOf(p provider.Provider) string {
	if p.API == "" {
		return translate.OpenAI
	}
	return p.API
}

// translateRequest converts a caller's request of shape from to the other shape.
func translateRequest(body []byte, from string) ([]byte, error) {
	if from == translate.OpenAI {
		return translate.OpenAIToAnthropic(body)
	}
	return translate.AnthropicToOpenAI(body)
}

// maxTranslatedBody bounds a whole (non-streamed) response read for translation.
const maxTranslatedBody = 32 << 20

// relayTranslated answers the caller in its own shape (to) from a response in
// the provider's shape. The usage counter reads the provider's original bytes,
// which it already understands in both vocabularies.
func (a *api) relayTranslated(w http.ResponseWriter, resp *http.Response, connID, to string, stream bool) {
	for k, vs := range resp.Header {
		if hopByHop[k] || k == "Content-Length" || k == "Content-Type" || k == "Content-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	if resp.StatusCode < 400 && stream && isSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		tap := &respTap{headLimit: usageTapHeadLimit, tailLimit: usageTapTailLimit}
		src := io.TeeReader(resp.Body, tapWriter{tap})
		out := flushWriter{w: w, rc: http.NewResponseController(w)}
		if to == translate.OpenAI {
			translate.AnthropicStreamToOpenAI(out, src)
		} else {
			translate.OpenAIStreamToAnthropic(out, src)
		}
		a.recordUsage(connID, tap.bytes(), "")
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTranslatedBody))
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot read the upstream answer")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if resp.StatusCode >= 400 {
		w.WriteHeader(resp.StatusCode)
		w.Write(translate.Error(body, to))
		return
	}
	var out []byte
	if to == translate.OpenAI {
		out, err = translate.AnthropicResponseToOpenAI(body)
	} else {
		out, err = translate.OpenAIResponseToAnthropic(body)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot translate the upstream answer")
		return
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	a.recordUsage(connID, body, "")
}

// tapWriter feeds a respTap from a TeeReader.
type tapWriter struct{ t *respTap }

func (t tapWriter) Write(p []byte) (int, error) { t.t.write(p); return len(p), nil }

// flushWriter writes to the caller and flushes each write, so a translated
// stream reaches the caller as it is produced.
type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) { return f.w.Write(p) }

// Flush ignores a writer that cannot flush; the bytes still arrive at the end.
func (f flushWriter) Flush() error { f.rc.Flush(); return nil }

// anyAnthropic reports whether one of the accounts speaks the Anthropic shape.
func (a *api) anyAnthropic(targets []store.Connection) bool {
	for _, c := range targets {
		if p, ok := a.providerFor(c); ok && shapeOf(p) == translate.Anthropic {
			return true
		}
	}
	return false
}

// countTokensEstimate answers Anthropic's count_tokens for a provider that has
// no such endpoint, with the usual four characters per token. Clients use it to
// size a context, so an estimate serves them better than an error.
func countTokensEstimate(w http.ResponseWriter, body []byte) {
	writeJSON(w, map[string]any{"input_tokens": len(body)/4 + 1})
}
