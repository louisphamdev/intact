package httpapi

import (
	_ "embed"
	"net/http"
)

// openAPIDoc describes the /v1 surface. Keep it in step with the routes in
// newServer and the envelopes in writeAPIError; TestOpenAPIDocNamesEveryRoute
// fails when a /v1 route is missing from it.
//
//go:embed openapi.json
var openAPIDoc []byte

func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openAPIDoc)
}
