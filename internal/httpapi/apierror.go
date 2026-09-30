package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/louisphamdev/intact/internal/translate"
)

// apiWriter marks a /v1 response with the shape its caller reads, so every
// error intact raises there leaves in that API's own envelope. Dashboard and
// management routes keep {"error": "<message>"}.
type apiWriter struct {
	http.ResponseWriter
	shape string
}

func (w apiWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestIDHeaders carry one id per /v1 call. An upstream's own id replaces it
// when the answer is relayed, so a provider ticket can still quote it.
var requestIDHeaders = []string{"Request-Id", "X-Request-Id"}

// v1API wraps a /v1 handler: the error shape by path, a request id, and an
// answer to a CORS preflight, which /v1 does not serve.
func v1API(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shape := translate.OpenAI
		if isAnthropicPath(r.URL.Path) {
			shape = translate.Anthropic
		}
		b := make([]byte, 12)
		rand.Read(b)
		id := "req_" + hex.EncodeToString(b)
		for _, h := range requestIDHeaders {
			w.Header().Set(h, id)
		}
		aw := apiWriter{ResponseWriter: w, shape: shape}
		if r.Method == http.MethodOptions {
			writeError(aw, http.StatusMethodNotAllowed, "CORS preflight is not served: call /v1 from a server, not a browser")
			return
		}
		next(aw, r)
	}
}

// isAnthropicPath covers the Messages family, count_tokens included. The
// server has already collapsed a doubled /v1.
func isAnthropicPath(p string) bool {
	return p == "/v1/messages" || strings.HasPrefix(p, "/v1/messages/")
}

// writeAPIError answers with an error in the caller's envelope. code and param
// are OpenAI's optional fields; the Anthropic envelope has no such fields.
func writeAPIError(w http.ResponseWriter, status int, code, param, msg string) {
	aw, ok := findAPIWriter(w)
	if !ok {
		writeLegacyError(w, status, msg)
		return
	}
	var body any
	if aw.shape == translate.Anthropic {
		body = map[string]any{"type": "error", "error": map[string]any{"type": translate.AnthropicErrorType(status, ""), "message": msg}}
	} else {
		typ := "invalid_request_error"
		switch {
		case status == http.StatusTooManyRequests:
			typ = "requests"
			if code == "" {
				code = "rate_limit_exceeded"
			}
		case status == http.StatusUnauthorized:
			if code == "" {
				code = "invalid_api_key"
			}
		case status >= 500:
			typ = "server_error"
		}
		e := map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}
		if code != "" {
			e["code"] = code
		}
		if param != "" {
			e["param"] = param
		}
		body = map[string]any{"error": e}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(body)
	w.Write(b)
}

// findAPIWriter looks through writers that wrap the /v1 one.
func findAPIWriter(w http.ResponseWriter) (apiWriter, bool) {
	for {
		if aw, ok := w.(apiWriter); ok {
			return aw, true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return apiWriter{}, false
		}
		w = u.Unwrap()
	}
}
