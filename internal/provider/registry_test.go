package provider

import "testing"

func TestLookupReturnsClassBProviders(t *testing.T) {
	p, ok := Lookup("groq")
	if !ok {
		t.Fatal("groq is not registered")
	}
	if p.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if p.AuthHeader != "Authorization" || p.AuthPrefix != "Bearer " {
		t.Errorf("auth = %q %q, want Authorization / Bearer ", p.AuthHeader, p.AuthPrefix)
	}

	if _, ok := Lookup("antigravity"); ok {
		t.Error("antigravity must not be registered in phase 1: it is class A")
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("an unknown id must not resolve")
	}
}

// Class A means the request must look like the genuine tool. The values come from
// a capture of Claude Code 2.1.278 taken on 2026-09-20; see docs/class-a-claude.md.
func TestClaudeCarriesTheIdentityOfTheRealTool(t *testing.T) {
	p, ok := Lookup("claude")
	if !ok {
		t.Fatal("claude is not registered")
	}
	if p.BaseURL != "https://api.anthropic.com/v1" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	// Identity is what names the tool. A caller must never be able to replace it,
	// because the point of this provider is that the upstream sees Claude Code.
	for k, want := range map[string]string{
		"User-Agent": "claude-cli/2.1.278 (external, sdk-cli)",
		"X-App":      "cli",
	} {
		if p.Identity[k] != want {
			t.Errorf("Identity[%q] = %q, want %q", k, p.Identity[k], want)
		}
	}
	// Defaults are features, not identity. The caller decides them.
	if p.Defaults["Anthropic-Version"] != "2023-06-01" {
		t.Errorf("Anthropic-Version default = %q", p.Defaults["Anthropic-Version"])
	}
	if p.Defaults["Anthropic-Beta"] == "" {
		t.Error("Anthropic-Beta must have a default: without it the upstream refuses the oauth token")
	}
	if _, clash := p.Identity["Anthropic-Beta"]; clash {
		t.Error("Anthropic-Beta must not be identity: it selects features and the caller must keep control of it")
	}
}
