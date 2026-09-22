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
	// API is the request shape the upstream speaks: "anthropic", "typesafe", or
	// empty for OpenAI Chat Completions. intact translates between openai and
	// anthropic when the caller uses the other one.
	API string
	// Setup tells the dashboard what a new connection needs besides a label:
	// "key" (the default), "account" (a key and an account id), "none", or
	// "oauth" (an account signs in; it cannot be added with a pasted key).
	Setup string
	// Models is the list to use when the upstream has no model endpoint.
	Models []string
	// Exchange names how the stored credential becomes the bearer the upstream
	// takes: "copilot" trades a GitHub OAuth token for a Copilot token.
	Exchange string
	// RequestIDHeader, when set, carries a fresh random id on each request.
	RequestIDHeader string
	// AccountHeader, when set, carries the ChatGPT account id read from the
	// access token's claims.
	AccountHeader string
	// SessionHeader, when set, carries a stable id per connection.
	SessionHeader string
	// ModelsQuery is appended to the model list URL.
	ModelsQuery string
	// Watch turns on structure drift monitoring. It is set on the providers
	// reached as a real tool (OAuth, impersonated clients), whose formats move
	// with each tool release; a documented API does not need it.
	Watch bool
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
	// GitHub Copilot, as the VS Code Copilot Chat extension reaches it. The
	// stored credential is the GitHub OAuth token (gho_…); it is traded for a
	// short-lived Copilot token before each call (see internal/httpapi/copilot.go).
	"github": {
		ID:         "github",
		BaseURL:    "https://api.githubcopilot.com",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Exchange:   "copilot",
		Watch:      true,
		Identity: map[string]string{
			"Copilot-Integration-Id":              "vscode-chat",
			"Editor-Version":                      "vscode/" + CopilotVSCodeVersion,
			"Editor-Plugin-Version":               "copilot-chat/" + CopilotChatVersion,
			"User-Agent":                          "GitHubCopilotChat/" + CopilotChatVersion,
			"Openai-Intent":                       "conversation-panel",
			"X-Github-Api-Version":                CopilotAPIVersion,
			"X-Vscode-User-Agent-Library-Version": "electron-fetch",
			"X-Initiator":                         "user",
		},
		RequestIDHeader: "X-Request-Id",
	},
	// Codex, the ChatGPT backend the Codex CLI uses: the Responses API over
	// HTTP with server-sent events, with the ChatGPT OAuth access token.
	"codex": {
		ID:         "codex",
		BaseURL:    "https://chatgpt.com/backend-api/codex",
		API:        "responses",
		Watch:      true,
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Identity: map[string]string{
			"Originator": "codex_cli_rs",
			"User-Agent": "codex_cli_rs/" + CodexCLIVersion,
			"Version":    CodexCLIVersion,
		},
		Setup:         "oauth",
		AccountHeader: "ChatGPT-Account-ID",
		SessionHeader: "Session_id",
		ModelsQuery:   "client_version=" + CodexCLIVersion,
		Models: []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
			"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"},
	},
	// Antigravity: Google's Cloud Code Assist (v1internal) as the Antigravity
	// IDE reaches it, with the Google OAuth token. The Gemini request travels
	// inside an envelope naming the account's Cloud project.
	"antigravity": {
		ID:         "antigravity",
		BaseURL:    "https://daily-cloudcode-pa.googleapis.com",
		API:        "antigravity",
		Watch:      true,
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
		Identity:   map[string]string{"User-Agent": AntigravityUserAgent},
		Setup:      "oauth",
	},
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
		// Workers AI has no OpenAI-style /models; these are its chat models.
		Models: []string{
			"@cf/deepseek-ai/deepseek-r1-distill-qwen-32b", "@cf/meta/llama-3.1-70b-instruct-fp8-fast",
			"@cf/meta/llama-3.1-8b-instruct-awq", "@cf/meta/llama-3.1-8b-instruct-fp8-fast",
			"@cf/meta/llama-3.2-1b-instruct", "@cf/meta/llama-3.2-3b-instruct",
			"@cf/meta/llama-3.3-70b-instruct-fp8-fast", "@cf/mistralai/mistral-small-3.1-24b-instruct",
			"@cf/moonshotai/kimi-k2.5", "@cf/moonshotai/kimi-k2.6", "@cf/qwen/qwen2.5-coder-32b-instruct",
			"@cf/qwen/qwq-32b", "@cf/zai-org/glm-4.7-flash",
		},
	},
	// OpenCode Zen with a Zen API key. The keyless free tier is reserved for the
	// OpenCode app itself, so intact does not offer it.
	"opencode": {
		ID:         "opencode",
		BaseURL:    "https://opencode.ai/zen/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
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
		API:        "typesafe",
		Models:     []string{"jev-latest"},
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
	},
	"claude": {
		ID:         "claude",
		BaseURL:    "https://api.anthropic.com/v1",
		API:        "anthropic",
		Watch:      true,
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

// The Copilot Chat versions intact presents, from 9router's registry.
const (
	CopilotVSCodeVersion = "1.110.0"
	CopilotChatVersion   = "0.38.0"
	CopilotAPIVersion    = "2025-04-01"
)

// AntigravityUserAgent is the IDE identity Antigravity calls carry.
const (
	AntigravityVersion   = "2.11.0"
	AntigravityUserAgent = "antigravity/ide/" + AntigravityVersion + " darwin/arm64"
)

// CodexCLIVersion is the Codex CLI version intact presents; the backend hides
// models that need a newer client.
const CodexCLIVersion = "0.155.1"

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
