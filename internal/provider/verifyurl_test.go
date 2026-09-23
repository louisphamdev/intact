package provider

import "testing"

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
