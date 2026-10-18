package provider

import (
	"strings"
	"testing"
)

func TestNormalizeQuotaURL(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "/quota", "ftp://x.dev/q"} {
		d := Def{ID: "quotaco", BaseURL: "https://api.quotaco.dev/v1", QuotaURL: bad}
		if err := d.Normalize(); err == nil {
			t.Errorf("quotaUrl %q was accepted", bad)
		}
	}
	cases := []struct{ base, quota string }{
		{"https://api.quotaco.dev/v1", ""},
		{"https://api.quotaco.dev/v1", "http://127.0.0.1:8794/quota"},
		{"https://api.quotaco.dev/accounts/{accountId}/v1", "https://api.quotaco.dev/accounts/{accountId}/quota"},
	}
	for _, c := range cases {
		d := Def{ID: "quotaco", BaseURL: c.base, QuotaURL: " " + c.quota + " "}
		if err := d.Normalize(); err != nil {
			t.Errorf("quotaUrl %q: %v", c.quota, err)
		}
		if d.QuotaURL != c.quota {
			t.Errorf("quotaUrl %q was stored as %q", c.quota, d.QuotaURL)
		}
		if got := d.Provider().QuotaURL; got != c.quota {
			t.Errorf("Provider().QuotaURL = %q, want %q", got, c.quota)
		}
	}
}

// An account stores its id only when the base URL asks for one, so a quotaUrl
// that needs the id while the base URL does not would be called without it.
func TestNormalizeQuotaURLAccountIDNeedsTheBaseURLToAskForIt(t *testing.T) {
	d := Def{ID: "quotaco", BaseURL: "https://h/v1", QuotaURL: "https://h/accounts/{accountId}/quota"}
	if err := d.Normalize(); err == nil || !strings.Contains(err.Error(), "quotaUrl") {
		t.Errorf("Normalize = %v, want an error naming quotaUrl", err)
	}
}
