package provider

import (
	"fmt"
	"strings"
	"testing"
)

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

	if _, ok := Lookup("nope"); ok {
		t.Error("an unknown id must not resolve")
	}
}

// Anthropic refuses claude-opus-5-5 to a Claude Code older than 2.1.280
// (claude_code_version_too_old, 2026-10-01). Both identities use one version.
func TestClaudeIdentityIsNotTooOldForCurrentModels(t *testing.T) {
	p, _ := Lookup("claude")
	ua := p.Identity["User-Agent"]
	if !strings.Contains(ua, "claude-cli/"+ClaudeCLIVersion+" ") || !strings.Contains(ClaudeInteractiveUserAgent, "claude-cli/"+ClaudeCLIVersion+" ") {
		t.Errorf("User-Agent %q and %q must both carry ClaudeCLIVersion", ua, ClaudeInteractiveUserAgent)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(ClaudeCLIVersion, "%d.%d.%d", &major, &minor, &patch); err != nil || major*1e6+minor*1e3+patch < 2001280 {
		t.Errorf("ClaudeCLIVersion = %q, want 2.1.280 or newer", ClaudeCLIVersion)
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
		"User-Agent": "claude-cli/2.1.281 (external, sdk-cli)",
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

func TestNewProvidersRegistered(t *testing.T) {
	for _, id := range []string{"nvidia", "openrouter", "typesafe"} {
		p, ok := Lookup(id)
		if !ok {
			t.Errorf("provider %q not registered", id)
			continue
		}
		if p.BaseURL == "" || p.AuthHeader != "Authorization" || p.AuthPrefix != "Bearer " {
			t.Errorf("provider %q has an incomplete entry: %+v", id, p)
		}
	}
}

func TestBifrostMatchesEveryDeclaredClientBinary(t *testing.T) {
	// One provider, more than one client binary: `codex` and `codex exec` are the same
	// client and both send a credential from the provider, not from intact.
	p, ok := Lookup("codex")
	if !ok {
		t.Skip("no codex provider registered")
	}
	for _, ua := range []string{"codex_cli_rs/0.160.1", "codex_exec/0.160.1 (Debian 13.0.0; x86_64)"} {
		if !p.Bifrost(ua) {
			t.Errorf("Bifrost(%q) = false, want true", ua)
		}
	}
	for _, ua := range []string{"codex_exec", "xcodex_exec/1", " curl"} {
		if p.Bifrost(ua) {
			t.Errorf("Bifrost(%q) = true, want false", ua)
		}
	}
}
