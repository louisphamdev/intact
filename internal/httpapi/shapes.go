package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/drift"
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
	case translate.Antigravity:
		return s, "v1internal:streamGenerateContent?alt=sse"
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
	case translate.OpenAI, translate.Anthropic, translate.Responses, translate.Antigravity:
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
func alwaysStreams(shape string) bool {
	return shape == translate.Responses || shape == translate.Antigravity
}

// maxTranslatedBody bounds a whole (non-streamed) response read for translation.
const maxTranslatedBody = 32 << 20

// relayVia answers the caller in its own shape (to) from a response in the
// provider's shape (via). stream is what the caller asked for.
func (a *api) relayVia(w http.ResponseWriter, resp *http.Response, connID, to, via string, stream bool, provider, path string, cap *contract.Capture) {
	a.rate.capture(connID, resp.Header)
	for k, vs := range resp.Header {
		if hopByHop[k] || k == "Content-Length" || k == "Content-Type" || k == "Content-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Codex streams its events under a Content-Type of application/json, so a
	// shape that only ever streams is read as a stream whatever the header says.
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream") || alwaysStreams(via)
	if resp.StatusCode >= 400 || !isSSE {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxTranslatedBody))
		if cap != nil {
			if err != nil {
				close(cap.ProducerDone)
				_ = cap.Seal(false, nil, err)
			} else {
				cap.WriteResp(body)
				close(cap.ProducerDone)
			}
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "cannot read the upstream answer")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if resp.StatusCode >= 400 {
			w.WriteHeader(resp.StatusCode)
			_, writeErr := w.Write(translate.Error(body, to))
			if cap != nil {
				_ = cap.Seal(false, writeErr, nil)
			}
			return
		}
		out, err := wholeToClient(body, via, to)
		if err != nil {
			if cap != nil {
				_ = cap.Seal(false, nil, err)
			}
			writeError(w, http.StatusBadGateway, "cannot translate the upstream answer")
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(out)
		if cap != nil {
			_ = cap.Seal(false, writeErr, nil)
		}
		a.recordUsage(connID, body, "")
		if watched(provider) {
			a.drift.Observe(drift.Response, provider, path, body, false)
		}
		return
	}

	// Stream: provider events → chat chunks (in a goroutine) → the caller.
	// The provider's own bytes are tapped too, for the drift observer.
	raw := &respTap{headLimit: usageTapHeadLimit, tailLimit: usageTapTailLimit}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		if cap != nil {
			defer close(cap.ProducerDone)
		}
		var src io.Reader = resp.Body
		if cap != nil {
			src = io.TeeReader(resp.Body, captureWriter{cap: cap})
		}
		a.toChatChunks(pipeFlusher{pw}, io.TeeReader(src, tapWriter{raw}), via)
	}()
	tap := &respTap{headLimit: usageTapHeadLimit, tailLimit: usageTapTailLimit}
	chunks := io.TeeReader(pr, tapWriter{tap})
	out := flushWriter{w: w, rc: http.NewResponseController(w)}
	var clientErr error
	switch {
	case stream && to == translate.OpenAI:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		clientErr = copyFlushing(out, chunks)
	case stream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		ef := &errFlusher{flushWriter: out}
		translate.OpenAIStreamToAnthropic(ef, chunks)
		clientErr = ef.err
	default:
		whole := translate.CollectOpenAIStream(chunks)
		status := resp.StatusCode
		// A provider can stream an error event inside an HTTP 200. The collected
		// body is then an error, not an answer, so the caller must not see 200.
		if status < 400 && isErrorBody(whole) {
			status = http.StatusBadGateway
		}
		if to == translate.Anthropic {
			if b, err := translate.OpenAIResponseToAnthropic(whole); err == nil {
				whole = b
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, clientErr = w.Write(whole)
	}
	// Close the read end rather than drain it. On a normal finish the reader is
	// already at EOF; on a client disconnect a drain would block on the producer
	// still reading a slow upstream, so close it and let the deferred
	// resp.Body.Close release that goroutine.
	pr.Close()
	if cap != nil {
		_ = cap.Seal(true, clientErr, nil)
	}
	a.recordUsage(connID, tap.bytes(), "")
	if resp.StatusCode < 300 && watched(provider) {
		a.drift.Observe(drift.Response, provider, path, raw.bytes(), true)
	}
}

// isErrorBody reports whether a JSON body is an error envelope: a top-level
// "error" key, which a provider streams instead of a completion when it fails.
func isErrorBody(b []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m["error"]
	return ok
}

// toChatChunks converts a provider's event stream to Chat Completions chunks.
func (a *api) toChatChunks(dst translate.Flusher, src io.Reader, via string) {
	switch via {
	case translate.Antigravity:
		translate.GeminiStreamToOpenAI(dst, src, &a.sigs)
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

type captureWriter struct {
	cap *contract.Capture
}

func (c captureWriter) Write(p []byte) (int, error) {
	c.cap.WriteResp(p)
	return len(p), nil
}

type errFlusher struct {
	flushWriter
	err error
}

func (e *errFlusher) Write(p []byte) (int, error) {
	n, err := e.flushWriter.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func copyFlushing(dst flushWriter, src io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			dst.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
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
