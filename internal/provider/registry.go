// Package provider holds the upstream table. A class B provider has a documented
// API and is reached with a plain credential. A class A provider impersonates a
// real tool, so it is only added once that tool's traffic has been captured and
// the exact headers are known.
package provider

// Provider describes how to reach one upstream.
//
// Identity and Defaults differ in who wins. Identity names the tool and always
// replaces what the caller sent, because a class A upstream answers differently
// when it does not recognise the client. Defaults select features, so the caller
// keeps control and a default only fills a header that is absent.
type Provider struct {
	ID         string
	BaseURL    string
	AuthHeader string
	AuthPrefix string
	Identity   map[string]string
	Defaults   map[string]string
}

// Captured from Claude Code 2.1.278 on 2026-09-20. The upstream rejects an OAuth
// token without claude-code-20250219 and oauth-2025-04-20, so this list is part
// of the credential, not decoration. Details: docs/class-a-claude.md.
const claudeBeta = "claude-code-20250219,oauth-2025-04-20,context-1m-2025-08-07," +
	"interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13," +
	"context-management-2025-06-27,prompt-caching-scope-2026-01-05," +
	"mid-conversation-system-2026-04-07,mid-conversation-tool-changes-2026-07-01," +
	"advanced-tool-use-2025-11-20,mid-conversation-system-clear-at-2026-08-21," +
	"effort-2025-11-24,fallback-credit-2026-06-01,thinking-binding-controls-2026-08-01," +
	"extended-cache-ttl-2025-04-11,cache-diagnosis-2026-04-07"

var registry = map[string]Provider{
	"groq": {
		ID:         "groq",
		BaseURL:    "https://api.groq.com/openai/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
	},
	"claude": {
		ID:         "claude",
		BaseURL:    "https://api.anthropic.com/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Identity: map[string]string{
			"User-Agent": "claude-cli/2.1.278 (external, sdk-cli)",
			"X-App":      "cli",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
		},
		Defaults: map[string]string{
			"Anthropic-Version": "2023-06-01",
			"Anthropic-Beta":    claudeBeta,
		},
	},
}

// Lookup returns the provider with this id.
func Lookup(id string) (Provider, bool) {
	p, ok := registry[id]
	return p, ok
}
