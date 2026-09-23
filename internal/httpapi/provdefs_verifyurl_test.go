package httpapi

import (
	"path/filepath"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A def whose verifyUrl is a javascript: URL must not be stored: the dashboard
// puts that value in the href of the device sign-in link.
func TestPostProviderDefRejectsJavascriptVerifyURL(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	h := New(s, nil)
	def := `{"id":"devco","kind":"oauth-device","baseUrl":"https://api.devco.dev/v1","modelsUrl":"none","models":["m-1"],` +
		`"oauth":{"deviceCodeUrl":"https://auth.devco.dev/device","tokenUrl":"https://auth.devco.dev/token","clientId":"cid","verifyUrl":"javascript:alert(1)"}}`
	if rec := postJSONTo(h, "/provider-defs", def); rec.Code != 400 {
		t.Fatalf("POST /provider-defs = %d %s, want 400", rec.Code, rec.Body.String())
	}
}
