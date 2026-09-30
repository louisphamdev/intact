package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestOpenAPIDocNamesEveryRoute(t *testing.T) {
	h, _ := v1Env(t, "http://127.0.0.1:9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/openapi.json", nil))
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &doc) != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("GET /openapi.json = %d %s", rec.Code, rec.Body.String())
	}
	for path, method := range map[string]string{
		"/v1/models": "get", "/v1/models/{model}": "get", "/v1/chat/completions": "post",
		"/v1/messages": "post", "/v1/messages/count_tokens": "post", "/v1/responses": "post",
	} {
		if doc.Paths[path][method] == nil {
			t.Errorf("the document lacks %s %s", method, path)
		}
	}
}
