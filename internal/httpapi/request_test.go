package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
)

// loopbackRequest builds a test request the way a client on this machine sends
// it. httptest.NewRequest fixes the Host at example.com, which the guard in
// origin.go refuses on a server with no auth. Tests of the guard itself set
// their own Host; see origin_test.go.
func loopbackRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = "127.0.0.1:20130"
	return r
}
