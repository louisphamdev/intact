package httpapi

import (
	"bytes"
	"io"
	"testing"

	"github.com/louisphamdev/intact/internal/translate"
)

type cutReader struct{ done bool }

func (r *cutReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
}

type bufFlusher struct{ bytes.Buffer }

func (b *bufFlusher) Flush() error { return nil }

// A chat upstream cut mid-stream (or by the idle watchdog) must reach the next
// stage as an error, not as a clean end that reads as a finished answer.
func TestChatChunksPassOnAReadError(t *testing.T) {
	a := &api{}
	if err := a.toChatChunks(&bufFlusher{}, &cutReader{}, "", translate.Reply{}); err == nil {
		t.Fatal("a cut chat stream reported a clean end")
	}
}
