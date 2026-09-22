package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestErrorSignatureAndClass(t *testing.T) {
	a := errSignature(429, errMessage([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)))
	if a != "429 resource has been exhausted (e.g. check quota)." {
		t.Errorf("signature = %q", a)
	}
	// Ids and numbers do not split a group.
	x := errSignature(400, "Invalid value at 'request.contents[3].parts[0]' req_01ABCdefGHIjklMNOpqrSTU")
	y := errSignature(400, "Invalid value at 'request.contents[12].parts[1]' req_09ZYXwvuTSRqpoNMLkjiHGF")
	if x != y {
		t.Errorf("%q != %q", x, y)
	}
	for st, want := range map[int]string{0: ClassNetwork, 401: ClassAuth, 403: ClassAuth, 400: ClassRejected, 429: ClassRateLimit, 503: ClassServer, 504: ClassTimeout} {
		if got := classOf(st); got != want {
			t.Errorf("classOf(%d) = %s", st, got)
		}
	}
	q := AccountQuota{Windows: []QuotaWindow{{Name: "model gemini-3.8-flash", UsedPct: 4}, {Name: "model claude-opus-4-6-thinking", UsedPct: 100}}}
	if l := quotaLeft(q, "gemini-3.8-flash-high"); l < 0.95 || l > 0.97 {
		t.Errorf("left = %v", l)
	}
	if l := quotaLeft(AccountQuota{Windows: []QuotaWindow{{Name: "5h", UsedPct: 30}, {Name: "week", UsedPct: 80}}}, "gpt-5"); l < 0.19 || l > 0.21 {
		t.Errorf("tightest left = %v", l)
	}
	if l := quotaLeft(AccountQuota{}, "m"); l != -1 {
		t.Errorf("unknown left = %v", l)
	}
}

func TestErrorsAreKeptAndServed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"Unknown field anti_cheat"}}`)
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	rec := postV1(h, `{"model":"groq/llama","anti_cheat":1,"messages":[{"role":"user","content":"hi"}]}`)
	// The client still gets the provider's answer untouched.
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Unknown field anti_cheat") {
		t.Fatalf("relayed %d %s", rec.Code, rec.Body.String())
	}
	list, _ := s.ListUpstreamErrors(store.ErrorFilter{})
	if len(list) != 1 || list[0].Class != ClassRejected || list[0].Provider != "groq" || !strings.Contains(list[0].Headers, `"Retry-After":"7"`) {
		t.Fatalf("kept %+v", list)
	}
	full, _ := s.GetUpstreamError(list[0].ID)
	if !strings.Contains(full.ReqBody, "anti_cheat") || !strings.Contains(full.RespBody, "Unknown field") {
		t.Errorf("bodies %+v", full)
	}
	for _, path := range []string{"/errors", "/errors/stats"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path+"?provider=groq", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "unknown field anti_cheat") && !strings.Contains(rec.Body.String(), "Unknown field anti_cheat") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
