package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoRetriesOnTransientStatuses(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("body"))
	resp, err := Do(context.Background(), req, 3)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestDoDoesNotRetryOnClientError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("body"))
	resp, err := Do(context.Background(), req, 3)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1: a 400 is the caller's fault and must not be retried", calls)
	}
}

// An HTTP/2 connection that dies in silence must not take every later request
// with it (router-for-me/CLIProxyAPI#6534): the transport pings an idle
// connection and closes it when the ping goes unanswered.
func TestUpstreamTransportPingsIdleHTTP2Connections(t *testing.T) {
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatalf("upstream transport has no HTTP/2 health check: %#v", client.Transport)
	}
	if !tr.ForceAttemptHTTP2 || tr.Proxy == nil {
		t.Fatal("the transport lost the defaults of http.DefaultTransport")
	}
}
