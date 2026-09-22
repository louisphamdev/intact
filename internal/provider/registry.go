// Package provider holds the upstream table. A class B provider has a documented
// API and is reached with a plain credential. A class A provider impersonates a
// real tool, so it is only added once that tool's traffic has been captured and
// the exact headers are known.
package provider

import (
	"sort"
	"strings"
)

// Provider describes how to reach one upstream.
//
// Identity and Defaults differ in who wins. Identity names the tool and always
// replaces what the caller sent, because a class A upstream answers differently
// when it does not recognise the client. Defaults select features, so the caller
// keeps control and a default only fills a header that is absent.
//
// An empty AuthHeader means the upstream takes no credential. A BaseURL that
// holds {accountId} is completed per connection (see Setup).
type Provider struct {
	ID         string
	BaseURL    string
	AuthHeader string
	AuthPrefix string
	Identity   map[string]string
	Defaults   map[string]string
	// Setup tells the dashboard what a new connection needs besides a label:
	// "key" (the default), "account" (a key and an account id) or "none".
	Setup string
}

// Generic is an OpenAI-compatible upstream that a connection defines itself
// with its own id and base URL, such as a self-hosted or niche gateway.
func Generic(id, baseURL string) Provider {
	return Provider{ID: id, BaseURL: baseURL, AuthHeader: "Authorization", AuthPrefix: "Bearer "}
}

// WithAccount completes a BaseURL that holds {accountId}.
func (p Provider) WithAccount(accountID string) string {
	return strings.ReplaceAll(p.BaseURL, "{accountId}", accountID)
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
	// Class B, documented OpenAI-compatible APIs: a plain Bearer credential.
	"nvidia": {
		ID:         "nvidia",
		BaseURL:    "https://integrate.api.nvidia.com/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
	},
	// Cloudflare Workers AI serves an OpenAI-compatible API under the account.
	"cloudflare-ai": {
		ID:         "cloudflare-ai",
		BaseURL:    "https://api.cloudflare.com/client/v4/accounts/{accountId}/ai/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Setup:      "account",
	},
	// OpenCode Zen's free models take no key; the client header is what the
	// desktop app sends.
	"opencode": {
		ID:       "opencode",
		BaseURL:  "https://opencode.ai/zen/v1",
		Identity: map[string]string{"X-Opencode-Client": "desktop"},
		Setup:    "none",
	},
	"openrouter": {
		ID:         "openrouter",
		BaseURL:    "https://openrouter.ai/api/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
	},
	// TypeSafe AI is not OpenAI-shaped (POST /systemone with a state+questions
	// body), but it uses a Bearer token and returns a usage object, so the
	// passthrough carries it without a translation layer. Captured from
	// docs.typesafe.ai on 2026-09-21.
	"typesafe": {
		ID:         "typesafe",
		BaseURL:    "https://api.typesafe.ai/v1",
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

// IDs returns the registered provider ids in sorted order.
func IDs() []string {
	out := make([]string, 0, len(registry))
	for id := range registry {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
