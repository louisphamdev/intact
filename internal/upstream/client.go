// Package upstream performs one provider call and applies the retry policy.
package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// The call limits. A proxied stream can send for hours, so DoStream bounds the
// wait for the headers and the gap between two reads instead of the whole call.
// Every other caller keeps Total on the whole call.
var (
	Total        = NewLimit(10 * time.Minute)
	StreamHeader = NewLimit(10 * time.Minute)
	StreamIdle   = NewLimit(5 * time.Minute)
)

// Limit is one call bound. Set makes it safe to shorten in a test while other
// calls run; production keeps the declared value.
type Limit struct{ ns atomic.Int64 }

func NewLimit(d time.Duration) *Limit {
	l := &Limit{}
	l.ns.Store(int64(d))
	return l
}

// Set stores d and returns the function that puts the old value back.
func (l *Limit) Set(d time.Duration) func() {
	old := l.ns.Swap(int64(d))
	return func() { l.ns.Store(old) }
}

func (l *Limit) Get() time.Duration { return time.Duration(l.ns.Load()) }

var client = &http.Client{}

// retryable lists the statuses that mean "the upstream is busy, ask again".
// A 4xx other than 429 is the caller's fault and repeating it only wastes quota.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusServiceUnavailable
}

// Do sends req, retrying a transient failure up to attempts times. Total bounds
// the whole call, the body the caller reads included.
func Do(ctx context.Context, req *http.Request, attempts int) (*http.Response, error) {
	return do(ctx, req, attempts, Total.Get(), 0)
}

// DoStream sends req like Do, for a response the caller relays. It waits
// StreamHeader for the headers, then bounds the gap between two reads of the
// body with StreamIdle. A stream that keeps sending is never cut.
func DoStream(ctx context.Context, req *http.Request, attempts int) (*http.Response, error) {
	return do(ctx, req, attempts, StreamHeader.Get(), StreamIdle.Get())
}

// do runs the attempts under one deadline of bound. When idle is above zero the
// deadline moves to idle once the headers are in, and every read pushes it back.
func do(ctx context.Context, req *http.Request, attempts int, bound, idle time.Duration) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	// The cause names a deadline, so a call that passed its limit reads as a
	// timeout and not as a caller that went away.
	deadline := time.AfterFunc(bound, func() { cancel(context.DeadlineExceeded) })
	// The deadline must outlive this function: it bounds the body the caller
	// has not read yet. boundedBody stops it, so every return that has no body
	// to hand back stops it here instead.
	give := func(resp *http.Response, err error) (*http.Response, error) {
		if resp == nil {
			deadline.Stop()
			cancel(nil)
			return nil, err
		}
		resp.Body = &boundedBody{rc: resp.Body, deadline: deadline, cancel: cancel, idle: idle}
		if idle > 0 {
			deadline.Reset(idle)
		}
		return resp, err
	}

	var resp *http.Response
	var err error
	for i := 0; i < attempts; i++ {
		attempt := req.Clone(ctx)
		if body != nil {
			attempt.Body = io.NopCloser(bytes.NewReader(body))
			attempt.ContentLength = int64(len(body))
		}
		resp, err = client.Do(attempt)
		if err == nil && !retryable(resp.StatusCode) {
			return give(resp, nil)
		}
		if resp != nil && i < attempts-1 {
			resp.Body.Close()
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return give(nil, context.Cause(ctx))
			case <-time.After(time.Duration(1<<i) * time.Second):
			}
		}
	}
	return give(resp, err)
}

// boundedBody holds the deadline of its call. Close releases it. For a stream
// (idle above zero) every read that brings bytes pushes the deadline back, so
// only a silent upstream ends the call.
type boundedBody struct {
	rc       io.ReadCloser
	deadline *time.Timer
	cancel   context.CancelCauseFunc
	idle     time.Duration
	once     sync.Once
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.idle > 0 {
		b.deadline.Reset(b.idle)
	}
	return n, err
}

func (b *boundedBody) Close() error {
	b.once.Do(func() {
		b.deadline.Stop()
		b.cancel(nil)
	})
	return b.rc.Close()
}
