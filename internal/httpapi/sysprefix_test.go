package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/louisphamdev/intact/internal/provider"
)

func TestWithSystemPrefix(t *testing.T) {
	p := provider.Provider{SystemPrefix: "ID."}
	id := `{"type":"text","text":"ID."}`
	for in, want := range map[string]string{
		`{"model":"m","messages":[]}`:                      `{"system":[` + id + `],"model":"m","messages":[]}`,
		`{"model":"m","system":"Be brief.","messages":[]}`: `{"model":"m","system":[` + id + `,{"type":"text","text":"Be brief."}],"messages":[]}`,
		`{"model":"m","system":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}],"messages":[]}`: `{"model":"m","system":[` + id + `,{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
		`{"model":"m","system":"ID. and more","messages":[]}`:                                                    `{"model":"m","system":"ID. and more","messages":[]}`,
		`{"model":"m","system":[{"type":"text","text":"ID."}],"messages":[]}`:                                    `{"model":"m","system":[{"type":"text","text":"ID."}],"messages":[]}`,
		`{"model":"m","system":[],"messages":[]}`:                                                                `{"model":"m","system":[` + id + `],"messages":[]}`,
		`{"model":"m","input":"x"}`:                                                                              `{"model":"m","input":"x"}`,
	} {
		got := string(withSystemPrefix(p, []byte(in)))
		if got != want {
			t.Errorf("in  %s\ngot  %s\nwant %s", in, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("invalid JSON: %s", got)
		}
	}
	if got := string(withSystemPrefix(provider.Provider{}, []byte(`{"messages":[]}`))); got != `{"messages":[]}` {
		t.Errorf("no prefix changed the body: %s", got)
	}
}
