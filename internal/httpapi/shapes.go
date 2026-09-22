package httpapi

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

// A request reaches a provider in the provider's own shape. When the caller
// speaks the same shape, the bytes pass through untouched both ways; that is
// the point of intact. Only when the shapes differ is the request translated,
// with OpenAI Chat Completions as the hub: caller → chat → provider on the way
// in, provider → chat chunks → caller on the way out.

// clientShape names the shape of a caller's request from its path, or "" for a
// path intact forwards untouched (embeddings, responses, …).
func clientShape(path string) string {
	switch strings.Trim(path, "/") {
	case "chat/completions":
		return translate.OpenAI
	case "messages":
		return translate.Anthropic
	}
	return ""
}

// shapeOf names the request shape a provider speaks by default.
func shapeOf(p provider.Provider) string {
	if p.API == "" {
		return translate.OpenAI
	}
	return p.API
}

// copilotResponsesModels are the Copilot models learned to answer only on
// /responses: Copilot says so with a 400, and the call is retried there.
var copilotResponsesModels sync.Map

var copilotClaude = regexp.MustCompile(`(?i)claude`)

// shapeFor names the shape and endpoint path a provider takes for one model.
func shapeFor(p provider.Provider, model string) (shape, path string) {
	if p.Exchange == "copilot" {
		if copilotClaude.MatchString(model) {
			return translate.Anthropic, "v1/messages"
		}
		if _, ok := copilotResponsesModels.Load(model); ok {
			return translate.Responses, "responses"
		}
	}
	switch s := shapeOf(p); s {
	case translate.Anthropic:
		return s, "messages"
	case translate.Responses:
		return s, "responses"
	case translate.OpenAI:
		return s, "chat/completions"
	default:
		return s, ""
	}
}

// translatable reports whether intact can reach a provider shape from a caller.
func translatable(shape string) bool {
	switch shape {
	case translate.OpenAI, translate.Anthropic, translate.Responses:
		return true
	}
	return false
}

// toProvider converts a caller's request to the provider's shape.
func toProvider(body []byte, client, want string) ([]byte, error) {
	hub := body
	if client == translate.Anthropic {
		var err error
		if hub, err = translate.AnthropicToOpenAI(body); err != nil {
			return nil, err
		}
	}
	switch want {
	case translate.Anthropic:
		return translate.OpenAIToAnthropic(hub)
	case translate.Responses:
		return translate.OpenAIToResponses(hub)
	}
	return hub, nil
}

// alwaysStreams reports whether a provider shape is only ever called streaming.
func alwaysStreams(shape string) bool { return shape == translate.Responses }

// maxTranslatedBody bounds a whole (non-streamed) response read for translation.
const maxTranslatedBody = 32 << 20

// relayVia answers the caller in its own shape (to) from a response in the
// provider's shape (via). stream is what the caller asked for.
func (a *api) relayVia(w http.ResponseWriter, resp *http.Response, connID, to, via string, stream bool) {
	for k, vs := range resp.Header {
		if hopByHop[k] || k == "Content-Length" || k == "Content-Type" || k == "Content-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	if resp.StatusCode >= 400 || !isSSE {
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
		out, err := wholeToClient(body, via, to)
		if err != nil {
			writeError(w, http.StatusBadGateway, "cannot translate the upstream answer")
			return
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
		a.recordUsage(connID, body, "")
		return
	}

	// Stream: provider events → chat chunks (in a goroutine) → the caller.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		toChatChunks(pipeFlusher{pw}, resp.Body, via)
	}()
	tap := &respTap{headLimit: usageTapHeadLimit, tailLimit: usageTapTailLimit}
	chunks := io.TeeReader(pr, tapWriter{tap})
	out := flushWriter{w: w, rc: http.NewResponseController(w)}
	switch {
	case stream && to == translate.OpenAI:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		copyFlushing(out, chunks)
	case stream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		translate.OpenAIStreamToAnthropic(out, chunks)
	default:
		whole := translate.CollectOpenAIStream(chunks)
		if to == translate.Anthropic {
			if b, err := translate.OpenAIResponseToAnthropic(whole); err == nil {
				whole = b
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(whole)
	}
	io.Copy(io.Discard, pr)
	a.recordUsage(connID, tap.bytes(), "")
}

// toChatChunks converts a provider's event stream to Chat Completions chunks.
func toChatChunks(dst translate.Flusher, src io.Reader, via string) {
	switch via {
	case translate.Anthropic:
		translate.AnthropicStreamToOpenAI(dst, src)
	case translate.Responses:
		translate.ResponsesStreamToOpenAI(dst, src)
	default:
		io.Copy(dst, src)
	}
}

// wholeToClient converts a whole (non-streamed) response.
func wholeToClient(body []byte, via, to string) ([]byte, error) {
	hub := body
	if via == translate.Anthropic {
		var err error
		if hub, err = translate.AnthropicResponseToOpenAI(body); err != nil {
			return nil, err
		}
	}
	if to == translate.Anthropic {
		return translate.OpenAIResponseToAnthropic(hub)
	}
	return hub, nil
}

func copyFlushing(dst flushWriter, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			dst.Flush()
		}
		if err != nil {
			return
		}
	}
}

// pipeFlusher lets a stream converter write into a pipe.
type pipeFlusher struct{ w *io.PipeWriter }

func (p pipeFlusher) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeFlusher) Flush() error                { return nil }

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
