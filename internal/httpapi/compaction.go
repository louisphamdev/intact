package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

// compaction answers a Codex compaction trigger.
//
// The provider is asked for the summary as an ordinary completion, because that is the one thing
// every provider can do, and the answer is then put back into the shape Codex accepts: exactly
// one compaction output item. Getting that item wrong is what makes a thread unrecoverable, so
// the summary is never empty. A provider that answers with a tool call, or with nothing usable,
// is reported as an error instead, because a blank summary would replace the whole history with
// nothing -- which is worse than the failure it replaces.
//
// The summary is capped, because it replaces the history and an uncapped one would put the next
// turn straight back over the window. A summary that does not fit the cap is cut at a paragraph
// boundary rather than mid-sentence, so what survives reads as the start of an account of the
// work rather than as a torn one.
const compactionSummaryCap = 24 << 10

func (a *api) compaction(
	w http.ResponseWriter, r *http.Request, body []byte,
	targets []store.Connection, start int, model string, stream bool, cap *contract.Capture,
) {
	if cap != nil {
		defer cap.ReleaseOnAbort()
	}
	summaryBody := summaryRequest(body)
	if summaryBody == nil {
		writeError(w, http.StatusBadRequest, "the compaction request carries no input")
		return
	}
	resp, conn, err := a.askForSummary(r, summaryBody, targets, start, model)
	if err == nil && resp.StatusCode >= 400 {
		// An account that only speaks the streamed shape refuses a non-streamed request,
		// and a client that asked without a stream would otherwise be left with an error
		// where a summary should have been. The answer is read either way, so the one
		// thing worth changing is the flag.
		if body := withStream(summaryBody, true); body != nil {
			resp.Body.Close()
			if retry, again, rerr := a.askForSummary(r, body, targets, start, model); rerr == nil {
				resp, conn = retry, again
				err = nil
			}
		}
	}
	if err != nil {
		// The provider's own refusal is the caller's error: Codex shows it, and unlike a
		// malformed compaction item it leaves the thread resumable.
		writeError(w, http.StatusBadGateway, "no account could summarize this conversation: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTranslatedBody))
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot read the summary")
		return
	}
	if resp.StatusCode >= 400 {
		logCompactionFailed(conn.Provider, model, "the answer was "+statusCode(resp.StatusCode)+": "+firstLine(string(raw)))
		writeError(w, http.StatusBadGateway, conn.Provider+" could not summarize this conversation")
		return
	}
	// The client asked for a stream or did not, and the provider answered in that shape
	// because the summary request carried the client's own stream setting through.
	summary := compactionSummary(raw)
	if summary == "" && bytes.Contains(raw, []byte("data:")) {
		summary = compactionSummaryFromStream(raw)
	}
	if summary == "" {
		logCompactionFailed(conn.Provider, model, "the answer held no summary text")
		writeError(w, http.StatusBadGateway,
			"the provider answered the summary request with no text to store as the summary")
		return
	}
	summary = capSummary(summary)

	itemID, respID := compactionIDs()
	w.Header().Set("Content-Type", contentTypeFor(stream))
	if stream {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.Write(compactionStream(itemID, respID, summary, model))
		flush(w)
	} else {
		w.WriteHeader(http.StatusOK)
		w.Write(compactionBody(itemID, respID, summary, model))
	}
	logCompaction(conn.Provider, model, len(body), len(summary))
}

// capSummary keeps a summary inside the cap, preferring to end it at a paragraph and then at a
// sentence. Cut mid-word it would be a corrupted account of the work; cut at a boundary it is a
// shorter one.
func capSummary(s string) string {
	if len(s) <= compactionSummaryCap {
		return s
	}
	cut := s[:compactionSummaryCap]
	if i := strings.LastIndexByte(cut, '\n'); i > compactionSummaryCap/2 {
		return cut[:i]
	}
	if i := strings.LastIndexAny(cut, ".!?\n"); i > compactionSummaryCap/2 {
		return strings.TrimSpace(cut[:i+1])
	}
	return cut
}

// askForSummary sends the summary request to the accounts that can answer it, in the order a
// normal request would use them, so a thread that compacts does not also lose its account.
func (a *api) askForSummary(
	r *http.Request, body []byte, targets []store.Connection, start int, model string,
) (*http.Response, store.Connection, error) {
	attempts := a.attemptsFor(r.Context(), targets, start, body)
	if m, _ := bodyModel(body); m != "" {
		attempts = a.skipSpent(attempts, m)
	}
	var lastErr error
	for i, at := range attempts {
		conn := at.conn
		p, ok := a.providerFor(conn)
		if !ok {
			continue
		}
		secret, err := a.secretFor(r.Context(), conn.ID)
		if err != nil {
			continue
		}
		if secret, err = a.exchanged(r.Context(), p, conn.ID, secret); err != nil {
			continue
		}
		send, path := summarySend(r, p, conn, body, model, at.model)
		send = filterFor(a, r, p, conn.Provider, send)
		resp, err := a.send(r, p, conn.Provider, path, secret, send)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 && retryableStatus(resp.StatusCode) && i < len(attempts)-1 {
			// A busy provider is worth trying the next account for. One that answers with
			// an error is not: the error is the answer.
			resp.Body.Close()
			lastErr = &providerStatus{provider: conn.Provider, code: resp.StatusCode}
			continue
		}
		return resp, conn, nil
	}
	if lastErr == nil {
		lastErr = &providerStatus{}
	}
	return nil, store.Connection{}, lastErr
}

// summarySend puts the summary request into the shape the account speaks, and names the path to
// ask on. A provider that does not speak Responses gets the chat form of the same conversation,
// because asking for a summary must not be the thing that fails.
func summarySend(
	r *http.Request, p provider.Provider, conn store.Connection, body []byte, callerModel, atModel string,
) ([]byte, string) {
	model := callerModel
	if atModel != "" {
		model = atModel
	}
	want, path := shapeFor(p, model)
	if path == "" {
		path = "responses"
	}
	if want == translate.Responses || want == "" {
		return body, path
	}
	if want == translate.OpenAI {
		if chat, ok := responsesToChatInput(body); ok {
			return chat, "chat/completions"
		}
	}
	// Any other shape is asked through the same translation a normal turn uses, so the
	// summary request is a request the account has already proven it can answer.
	out, _, err := toProvider(body, translate.Responses, want, p.ID)
	if err != nil {
		// Untranslatable: the Responses form goes out as it is rather than nothing going
		// out at all, and the provider's own error is what the caller sees.
		return body, path
	}
	return out, path
}

// providerStatus is a provider's own refusal, kept so both the log and the caller name it.
type providerStatus struct {
	provider string
	code     int
}

func (e *providerStatus) Error() string {
	if e.provider == "" {
		return "no usable account"
	}
	return e.provider + " answered " + strconv.Itoa(e.code)
}

// responsesToChatInput turns a Responses input array into the chat messages a Chat Completions
// provider understands, so the summary request reaches one in the shape it speaks.
func responsesToChatInput(body []byte) ([]byte, bool) {
	var req struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Input    []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	if req.Messages != nil {
		return nil, false
	}
	msgs := make([]json.RawMessage, 0, len(req.Input))
	for _, item := range req.Input {
		var it struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Args   string `json:"arguments"`
			Output string `json:"output"`
		}
		if json.Unmarshal(item, &it) != nil {
			continue
		}
		switch {
		case it.CallID != "":
			msgs = append(msgs, rawMessage(map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": it.CallID, "type": "function",
					"function": map[string]any{"name": it.Name, "arguments": it.Args}}},
			}))
		case it.Output != "":
			msgs = append(msgs, rawMessage(map[string]any{
				"role": "tool", "tool_call_id": it.CallID, "name": it.Name, "content": it.Output,
			}))
		case it.Role == "assistant":
			msgs = append(msgs, rawMessage(map[string]any{"role": "assistant", "content": textOf(it.Content)}))
		default:
			msgs = append(msgs, rawMessage(map[string]any{"role": "user", "content": textOf(it.Content)}))
		}
	}
	if len(msgs) == 0 {
		return nil, false
	}
	out, err := marshalPlain(struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Stream   bool              `json:"stream"`
	}{req.Model, msgs, false})
	if err != nil {
		return nil, false
	}
	return out, true
}

// textOf joins the text of an input item's content blocks.
func textOf(blocks []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, c := range blocks {
		b.WriteString(c.Text)
	}
	return b.String()
}

func rawMessage(v map[string]any) json.RawMessage {
	b, err := marshalPlain(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func contentTypeFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}

// withStream sets the stream flag on a request body, for the one retry that needs it.
func withStream(body []byte, on bool) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	flag := "false"
	if on {
		flag = "true"
	}
	req["stream"] = json.RawMessage(flag)
	out, err := marshalPlain(req)
	if err != nil {
		return nil
	}
	return out
}

// firstLine is the start of a provider's own words, for the log. An error body from a provider
// is the only thing that says why a summary did not come back, and it is worth one line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// itoa names a status in a log line. The package's tests have a helper of this name
// that takes an int64 (an account id), so this one is statusCode to keep both.
func statusCode(n int) string { return strconv.Itoa(n) }
