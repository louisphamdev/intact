package httpapi

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestProxyRecordsUsageWithoutAlteringTheResponse(t *testing.T) {
	respBody := `{"model":"llama-3.3-70b-versatile","choices":[{"message":{"content":"OK"}}],` +
		`"usage":{"prompt_tokens":78,"completion_tokens":56,"total_tokens":134}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respBody))
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := s.CreateConnection("groq", "test", "gsk-abc")

	h := New(s, map[string]string{"groq": up.URL})
	req := loopbackRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"groq/llama-3.3-70b-versatile","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The tap must not touch the bytes the caller receives.
	if rec.Body.String() != respBody {
		t.Fatalf("response altered by the tap:\n got %s\nwant %s", rec.Body.String(), respBody)
	}

	rows, err := s.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Usage rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	today := usageDay(time.Now())
	if r.Day != today || r.ConnectionID != c.ID || r.Model != "llama-3.3-70b-versatile" {
		t.Errorf("row key = %+v, want day=%s conn=%s model=llama-3.3-70b-versatile", r, today, c.ID)
	}
	if r.InputTokens != 78 || r.OutputTokens != 56 || r.Requests != 1 {
		t.Errorf("row counts = %+v, want input=78 output=56 requests=1", r)
	}
}

// A response with no usage object must not create a usage row.
func TestProxyRecordsNothingWhenNoUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "test", "gsk-abc")

	h := New(s, map[string]string{"groq": up.URL})
	req := loopbackRequest("POST", "/v1/x", strings.NewReader(`{"model":"groq/m"}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	rows, _ := s.Usage()
	if len(rows) != 0 {
		t.Errorf("Usage rows = %d, want 0: %+v", len(rows), rows)
	}
}

// Bug B: when the upstream body is gzip-encoded (a client that sends
// Accept-Encoding: gzip gets a gzip response that Go does not auto-decompress),
// the tap must decompress before reading usage, and must not alter the bytes
// the caller receives.
func TestProxyRecordsUsageWhenGzipEncoded(t *testing.T) {
	var gzBody bytes.Buffer
	zw := gzip.NewWriter(&gzBody)
	zw.Write([]byte(`{"model":"claude-opus-4-8","usage":{"input_tokens":120,"output_tokens":45}}`))
	zw.Close()
	raw := gzBody.Bytes()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(raw)
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "test", "sk-abc")

	h := New(s, map[string]string{"claude": up.URL})
	rec := httptest.NewRecorder()
	req := loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`))
	// The client accepts gzip, so the proxy forwards it and Go does not
	// auto-decompress the upstream response. This is the case that broke usage.
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("caller bytes altered: the gzip body must pass through unchanged")
	}
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].InputTokens != 120 || rows[0].OutputTokens != 45 {
		t.Fatalf("usage from gzip body not recorded: %+v", rows)
	}
}

// Bug C: a stream larger than the tap limit keeps its usage in the final chunk.
// The tap must retain the tail (and the head, for Anthropic input), not only
// the head.
func TestProxyRecordsUsageOnStreamLargerThanTapLimit(t *testing.T) {
	oldHead, oldTail := usageTapHeadLimit, usageTapTailLimit
	usageTapHeadLimit, usageTapTailLimit = 256, 256
	defer func() { usageTapHeadLimit, usageTapTailLimit = oldHead, oldTail }()

	filler := strings.Repeat("event: ping\ndata: {\"type\":\"ping\"}\n\n", 40) // > 256 bytes
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":78,"output_tokens":1}}}` + "\n\n" +
		filler +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":56}}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(stream))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("claude", "test", "sk-abc")

	h := New(s, map[string]string{"claude": up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude/m"}`)))

	if rec.Body.String() != stream {
		t.Fatalf("caller stream altered")
	}
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].InputTokens != 78 || rows[0].OutputTokens != 56 {
		t.Fatalf("usage on large stream wrong: %+v", rows)
	}
}

// Bug (live-found): the proxy forwarded the client's Accept-Encoding, so a
// client asking for br/zstd got a compressed upstream body the tap could not
// read, and usage was lost. The proxy must ask the upstream for identity.
func TestProxyForcesIdentityEncodingUpstream(t *testing.T) {
	var gotAE string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","usage":{"prompt_tokens":78,"completion_tokens":51}}`))
	}))
	defer up.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "test", "gsk-abc")

	h := New(s, map[string]string{"groq": up.URL})
	req := loopbackRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"groq/m"}`))
	req.Header.Set("Accept-Encoding", "br, gzip, zstd") // what curl --compressed sends
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotAE != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity so the body is never compressed", gotAE)
	}
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].InputTokens != 78 || rows[0].OutputTokens != 51 {
		t.Fatalf("usage not recorded: %+v", rows)
	}
}
