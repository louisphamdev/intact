// Package upstream performs one provider call and applies the retry policy.
package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Minute}

// retryable lists the statuses that mean "the upstream is busy, ask again".
// A 4xx other than 429 is the caller's fault and repeating it only wastes quota.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusServiceUnavailable
}

// Do sends req, retrying a transient failure up to attempts times.
// The body is buffered once so a retry can replay it.
func Do(ctx context.Context, req *http.Request, attempts int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
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
			return resp, nil
		}
		if resp != nil && i < attempts-1 {
			resp.Body.Close()
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<i) * time.Second):
			}
		}
	}
	return resp, err
}
