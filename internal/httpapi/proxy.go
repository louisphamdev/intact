package httpapi

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
	"github.com/louisphamdev/intact/internal/usage"
)

// The tap keeps a bounded copy of one response to read the token counts. A
// stream carries its usage at the tail (OpenAI's final chunk, Anthropic's
// message_delta) while Anthropic's input count sits near the head, so the tap
// keeps both ends and drops the middle. This bounds memory on a large stream.
var (
	usageTapHeadLimit = 1 << 20
	usageTapTailLimit = 256 << 10
)

// hopByHop headers belong to a single transport hop and must not be forwarded.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// proxy forwards one request to the account named in the path.
//
// It never reads the body and never re-serializes the response. The caller
// already speaks the provider's own protocol; this handler only adds identity.
func (a *api) proxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	conn, err := a.connection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	p, ok := provider.Lookup(conn.Provider)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	secret, err := a.store.Secret(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}

	base := p.BaseURL
	if over, ok := a.baseOverride[conn.Provider]; ok {
		base = over
	}
	target := base + "/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad upstream target")
		return
	}

	// Copy the caller's headers, minus the ones that belong to this hop and minus
	// Authorization: the stored credential must win, so a caller can never send a
	// request on an account using a token of their own choosing.
	for k, vs := range r.Header {
		// Accept-Encoding is dropped here and forced to identity below: a caller
		// that asks for br or zstd would get a body the usage tap cannot read,
		// and the counts would be lost.
		if hopByHop[k] || k == "Authorization" || k == "Host" || k == "Accept-Encoding" {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	// Ask the upstream for an uncompressed body so the tap always reads plain
	// bytes, whatever the caller advertised.
	out.Header.Set("Accept-Encoding", "identity")
	// A default fills a header the caller left out; it never overrules a choice the
	// caller made. Identity is the opposite: it always wins, so the upstream sees
	// the tool this provider impersonates and not whoever called this proxy.
	for k, v := range p.Defaults {
		if out.Header.Get(k) == "" {
			out.Header.Set(k, v)
		}
	}
	for k, v := range p.Identity {
		out.Header.Set(k, v)
	}
	out.Header.Set(p.AuthHeader, p.AuthPrefix+secret)

	resp, err := upstream.Do(r.Context(), out, 3)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopByHop[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	tapped := streamBody(w, resp)
	a.recordUsage(id, tapped, resp.Header.Get("Content-Encoding"))
}

// recordUsage reads the token counts out of a response the proxy already sent
// and folds them into the daily counter. It never changes what the caller
// received, and a body with no usage adds no row.
//
// A client that sends Accept-Encoding: gzip gets a gzip body that Go does not
// auto-decompress, so the tap holds compressed bytes. The counter decompresses
// its own copy; the caller still gets the original bytes untouched.
func (a *api) recordUsage(connID string, body []byte, contentEncoding string) {
	if contentEncoding == "gzip" {
		if plain, err := gunzip(body); err == nil {
			body = plain
		} else {
			return
		}
	}
	c := usage.Parse(body)
	if !c.Found {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	// A failure must not affect the request the caller already has; the counter
	// is a convenience, not part of the proxy contract. Log it so a store fault
	// (a lock, a full disk) is visible instead of losing counts in silence.
	if err := a.store.AddUsage(day, connID, c.Model, c.InputTokens, c.OutputTokens); err != nil {
		log.Printf("record usage for connection %s: %v", connID, err)
	}
}

// gunzip decompresses a gzip body, bounded so a hostile stream cannot exhaust
// memory through the tap.
func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, int64(usageTapHeadLimit+usageTapTailLimit)))
}

// streamBody copies the upstream body to the caller and flushes each chunk as it
// arrives. A plain io.Copy would let the server buffer, which holds a streamed
// response until the upstream finishes and breaks every SSE client.
//
// It returns a bounded copy of the body so the usage counter can be read without
// a second upstream call. The tap only reads; the caller's bytes are written
// first and are never altered by it.
func streamBody(w http.ResponseWriter, resp *http.Response) []byte {
	rc := http.NewResponseController(w)
	tap := &respTap{headLimit: usageTapHeadLimit, tailLimit: usageTapTailLimit}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return tap.bytes()
			}
			tap.write(buf[:n])
			// A flush failure only means this writer cannot flush; keep copying.
			_ = rc.Flush()
		}
		if readErr != nil {
			return tap.bytes()
		}
	}
}

// respTap keeps the head and the tail of a response and drops the middle. Usage
// lives at one end or the other, so both ends together carry it while memory
// stays bounded to headLimit + tailLimit.
type respTap struct {
	head      bytes.Buffer
	tail      []byte
	headLimit int
	tailLimit int
	total     int
}

func (t *respTap) write(p []byte) {
	t.total += len(p)
	if room := t.headLimit - t.head.Len(); room > 0 {
		if room >= len(p) {
			t.head.Write(p)
		} else {
			t.head.Write(p[:room])
		}
	}
	t.tail = append(t.tail, p...)
	if len(t.tail) > t.tailLimit {
		t.tail = t.tail[len(t.tail)-t.tailLimit:]
	}
}

// bytes returns the captured body. When the response fit inside the head, that
// is the whole body. Otherwise it joins head and tail with a newline; a line
// split across the gap fails to parse and is skipped, which is harmless because
// usage is a whole line at one end.
func (t *respTap) bytes() []byte {
	if t.total <= t.headLimit {
		return t.head.Bytes()
	}
	out := make([]byte, 0, t.head.Len()+1+len(t.tail))
	out = append(out, t.head.Bytes()...)
	out = append(out, '\n')
	out = append(out, t.tail...)
	return out
}

// writeError replies with a JSON body that never names a credential.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":"` + msg + `"}`))
}

func (a *api) connection(id string) (store.Connection, error) {
	list, err := a.store.ListConnections()
	if err != nil {
		return store.Connection{}, err
	}
	for _, c := range list {
		if c.ID == id {
			return c, nil
		}
	}
	return store.Connection{}, errors.New("not found")
}
