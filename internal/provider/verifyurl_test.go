package provider

import (
	"strings"
	"testing"
)

// verifyUrl becomes the href of the device sign-in link, so a javascript: or
// data: value is executable. Normalize must refuse it.
func TestNormalizeRejectsNonHTTPVerifyURL(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "/relative"} {
		d := Def{ID: "devco", Kind: KindOAuthDevice, BaseURL: "https://api.devco.dev/v1", ModelsURL: "none",
			Models: []string{"m-1"},
			OAuth: &OAuth{DeviceCodeURL: "https://auth.devco.dev/device", TokenURL: "https://auth.devco.dev/token",
				ClientID: "cid", VerifyURL: bad}}
		if err := d.Normalize(); err == nil {
			t.Errorf("verifyUrl %q was accepted", bad)
		}
	}
}

func TestNormalizeAcceptsHTTPVerifyURLAndEmpty(t *testing.T) {
	for _, ok := range []string{"", "https://auth.devco.dev/activate"} {
		d := Def{ID: "devco", Kind: KindOAuthDevice, BaseURL: "https://api.devco.dev/v1", ModelsURL: "none",
			Models: []string{"m-1"},
			OAuth: &OAuth{DeviceCodeURL: "https://auth.devco.dev/device", TokenURL: "https://auth.devco.dev/token",
				ClientID: "cid", VerifyURL: ok}}
		if err := d.Normalize(); err != nil {
			t.Errorf("verifyUrl %q: %v", ok, err)
		}
	}
}

// replayReasoning adds reasoning_content to a Chat Completions body; on any other api it would do
// nothing, so a def that sets it there is refused rather than saved as a silent no-op.
func TestNormalizeReplayReasoningOnlyForOpenAIChat(t *testing.T) {
	for _, api := range []string{"", "openai", "anthropic", "responses", "typesafe"} {
		d := Def{ID: "deepco", API: api, BaseURL: "https://api.deepco.dev/v1", ReplayReasoning: true}
		err := d.Normalize()
		if ok := api == "" || api == "openai"; ok && err != nil {
			t.Errorf("api %q: %v", api, err)
		} else if !ok && (err == nil || !strings.Contains(err.Error(), "replayReasoning")) {
			t.Errorf("api %q: replayReasoning accepted (err %v)", api, err)
		}
	}
}
