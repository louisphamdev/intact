package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A gateway error (502, 504), an Anthropic overload (529), a timeout (408) and a
// spent balance (402) belong to one account: the next account can answer
// (BerriAI/litellm#46003). A request-shape 400 is the answer, and is not resent.
func TestAccountErrorsFailOverAndA400DoesNot(t *testing.T) {
	for _, code := range []int{402, 408, 502, 504, 529, 400} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					w.Write([]byte(`{"data":[]}`))
					return
				}
				calls++
				if r.Header.Get("Authorization") == "Bearer ka" {
					w.WriteHeader(code)
					w.Write([]byte(`{"error":{"message":"account a"}}`))
					return
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer up.Close()
			s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
			defer s.Close()
			s.CreateConnection("groq", "a", "ka")
			s.CreateConnection("groq", "b", "kb")
			h := New(s, map[string]string{"groq": up.URL})
			rec := postV1(h, `{"model":"groq/llama","stream":false}`)
			if code == 400 {
				if rec.Code != 400 || calls != 1 {
					t.Fatalf("400: code=%d calls=%d, want the 400 relayed after one call", rec.Code, calls)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want the next account's 200", rec.Code, rec.Body.String())
			}
		})
	}
}
