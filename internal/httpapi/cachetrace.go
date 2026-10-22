package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"net/http"

	"github.com/louisphamdev/intact/internal/usage"
)

// Cache trace: one JSON line per answered request, so the cache ratio can be
// split by client, tool version, key and session source. usage_key_daily keeps
// only a daily sum per key and model, which cannot say which client missed.
//
// Off unless INTACT_CACHE_TRACE names a file. It only reads what the proxy
// already holds and never changes a byte the caller receives.
var cacheTrace struct {
	once sync.Once
	mu   sync.Mutex
	f    *os.File
}

func cacheTraceFile() *os.File {
	cacheTrace.once.Do(func() {
		p := os.Getenv("INTACT_CACHE_TRACE")
		if p == "" {
			return
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			cacheTrace.f = f
		}
	})
	return cacheTrace.f
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// traceCache records one answered request. resp.Request is the outbound request,
// so its headers are what the provider was sent.
func (a *api) traceCache(resp *http.Response, connID, served string, body []byte, contentEncoding string) {
	f := cacheTraceFile()
	if f == nil || resp == nil || resp.Request == nil {
		return
	}
	if contentEncoding == "gzip" {
		plain, err := io.ReadAll(io.LimitReader(mustGzip(body), 4<<20))
		if err != nil {
			return
		}
		body = plain
	}
	c := usage.Parse(body)
	if !c.Found {
		return
	}
	out := resp.Request
	model := served
	if model == "" {
		model = c.Model
	}
	skey, _ := out.Context().Value(sessionKeyCtx{}).(string)
	kind := ""
	if len(skey) >= 2 {
		kind = skey[:2]
	}
	rec := map[string]any{
		"t":          time.Now().Format(time.RFC3339),
		"key":        clientOf(out),
		"conn":       short(connID),
		"model":      model,
		"path":       out.URL.Path,
		"status":     resp.StatusCode,
		"in":         c.InputTokens,
		"cached":     c.CachedTokens,
		"out":        c.OutputTokens,
		"ua":         out.Header.Get("User-Agent"),
		"sent_len":   out.ContentLength,
		"affinity":   kind, // "s:" a named session, "c:" conversation head only, "" none
		"h_cc_sess":  short(out.Header.Get("X-Claude-Code-Session-Id")),
		"h_sess":     short(out.Header.Get("Session-Id")),
		"h_thread":   short(out.Header.Get("Thread-Id")),
		"h_x_sess":   short(out.Header.Get("X-Session-Id")),
		"h_opencode": short(out.Header.Get("X-Opencode-Session")),
		"retry_hdr":  resp.Header.Get("Retry-After") != "",
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	cacheTrace.mu.Lock()
	defer cacheTrace.mu.Unlock()
	f.Write(append(line, '\n'))
}

func mustGzip(b []byte) io.Reader {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return strings.NewReader("")
	}
	return zr
}
