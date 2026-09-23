package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// A stream that keeps sending must reach the caller whole, however long it
// runs. The old whole-call bound cut it in the middle.
func TestV1StreamOutlivesTheWholeCallLimit(t *testing.T) {
	defer upstream.Total.Set(200 * time.Millisecond)()
	defer upstream.StreamIdle.Set(2 * time.Second)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": srv.URL})

	rec := postV1(h, `{"model":"groq/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for i := 0; i < 6; i++ {
		if !strings.Contains(body, fmt.Sprintf(`{"n":%d}`, i)) {
			t.Errorf("event %d missing from %q", i, body)
		}
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("final event missing from %q", body)
	}
}

// A stream that stops sending must end on the idle deadline, not hang.
func TestV1StalledStreamEndsOnTheIdleLimit(t *testing.T) {
	defer upstream.StreamIdle.Set(300 * time.Millisecond)()
	defer upstream.StreamHeader.Set(5 * time.Second)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"n\":0}\n\n")
		w.(http.Flusher).Flush()
		// Stall: no byte until the caller gives up, or the test ends.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": srv.URL})

	start := time.Now()
	rec := postV1(h, `{"model":"groq/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	took := time.Since(start)
	if took > 1300*time.Millisecond {
		t.Errorf("the stalled request took %v, want under the idle limit + 1 s", took)
	}
	if !strings.Contains(rec.Body.String(), `{"n":0}`) {
		t.Errorf("the bytes sent before the stall are missing from %q", rec.Body.String())
	}
}

// Every caller that is not the proxy send keeps a bound on the whole call.
func TestModelListKeepsAWholeCallBound(t *testing.T) {
	defer upstream.Total.Set(300 * time.Millisecond)()

	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	// The gate opens before the server closes, so no handler holds Close back.
	defer close(stop)

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "a", "ka")
	a, _ := newServer(s, map[string]string{"groq": srv.URL}, nil)

	start := time.Now()
	resp, err := a.getModels(context.Background(), c)
	if err == nil {
		resp.Body.Close()
		t.Fatal("getModels returned no error, want the whole-call bound to end it")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("getModels took %v, want it bounded by the limit", took)
	}
}

// An upstream that never sends its headers ends on the header limit, and the
// error log still calls that a timeout.
func TestV1SilentUpstreamEndsOnTheHeaderLimit(t *testing.T) {
	defer upstream.StreamHeader.Set(300 * time.Millisecond)()

	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stop)

	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "a", "ka")
	h := New(s, map[string]string{"groq": srv.URL})

	start := time.Now()
	postV1(h, `{"model":"groq/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the silent upstream took %v, want the header limit to end it", took)
	}
	list, _ := s.ListUpstreamErrors(store.ErrorFilter{})
	if len(list) != 1 {
		t.Fatalf("stored errors = %d, want 1", len(list))
	}
	if list[0].Class != ClassTimeout {
		t.Errorf("class = %q, want %q: %s", list[0].Class, ClassTimeout, list[0].Message)
	}
}
