# intact

intact is a credential proxy for LLM providers. It keeps every account's
credential on one host and serves them all behind one base URL. A request goes
to the provider as intact received it, and the provider's answer comes back in
full: intact does not drop fields a gateway does not know.

It was built as a lighter, more faithful alternative to a large gateway, for one
operator who holds many accounts (API keys and the OAuth sign-ins of coding
tools) and wants to use them all from any client.

## Features

- **One base URL.** Every client points at `/v1`. The `model` in the body picks
  the provider and its accounts: `groq/llama-3.3-70b-versatile`, or a bare id
  that any provider lists. [Routing](docs/routing.md)
- **OpenAI and Anthropic shapes.** `/v1/chat/completions` and `/v1/messages`
  reach any provider. intact translates the request and the answer (streamed or
  whole) when the shapes differ. When they match, the bytes pass through untouched.
- **Rotation and failover.** Requests rotate across a model's accounts. A busy
  answer (429, 500, 503, 409) moves to the next account. An expired OAuth token
  is refreshed and the request retried.
- **Providers.** API-key providers (Groq, NVIDIA, OpenRouter, Cloudflare Workers
  AI, TypeSafe), the sign-ins of coding tools (Claude Code, Codex, Antigravity,
  GitHub Copilot), and any custom OpenAI, Anthropic or Responses endpoint.
  [Providers](docs/providers.md)
- **Model management.** A table per provider shows each model with an on/off
  switch, its thinking support and effort levels, and its last test.
  - Lists are fetched every hour.
  - Auto test keeps on only the models that answer.
  - Only free keeps on only the free models.
  - Antigravity's per-level models are folded into one model, picked by effort.

  [Models](docs/models.md)
- **Blacklist.** Editable rules remove what a provider refuses (a body field, a
  tool-schema key, a system-prompt line, a header) before the request leaves.
  [Blacklist](docs/blacklist.md)
- **Drift alerts.** For providers reached as a coding tool, intact learns the
  structure of requests and answers and records when a field appears,
  disappears or changes type. [Drift](docs/drift.md)
- **Quota and usage.** Quota is read from each provider (rolling windows, weekly
  pools, per-model shares). Token usage is counted per account, model and day.
  [Quota and usage](docs/quota-and-usage.md)
- **Management API and MCP.** Everything the dashboard does is available at
  `/api/*`, and to an agent through an MCP server at `/mcp`. [API](docs/api.md)
- **Dashboard.** A single embedded page with sign-in by TOTP code (no password).
  API keys are created there. [Security](docs/security.md)

## Quick start

```bash
CGO_ENABLED=0 go build -o intact ./cmd/intact
./intact -enroll                      # prints INTACT_TOTP_SECRET and an otpauth:// URI
export INTACT_TOTP_SECRET=...         # add the URI to an authenticator app
./intact -db ./intact.db -addr 127.0.0.1:20142
```

Open the dashboard, sign in with the code, add an account under **Providers**,
create an API key under **Endpoint → API keys**, then:

```bash
curl http://127.0.0.1:20142/v1/chat/completions \
  -H "Authorization: Bearer $INTACT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"groq/llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}'
```

[Getting started](docs/getting-started.md) covers configuration and a systemd
and Cloudflare Tunnel deployment.

## Documentation

| Page | Contents |
| --- | --- |
| [Getting started](docs/getting-started.md) | Build, flags, environment, first account, deployment |
| [Routing](docs/routing.md) | Model resolution, translation, rotation, failover, variants |
| [Providers](docs/providers.md) | Every built-in provider, sign-in flows, custom endpoints |
| [Models](docs/models.md) | The model table, fetching, switches, tests, auto test, only free, thinking |
| [Blacklist](docs/blacklist.md) | Request filters: kinds, scope, seeded rules |
| [Drift](docs/drift.md) | Structure learning and change alerts |
| [Quota and usage](docs/quota-and-usage.md) | Quota sources, the Quota page, usage counting |
| [API](docs/api.md) | Every HTTP endpoint and MCP tool |
| [Security](docs/security.md) | Sign-in, API keys, where credentials live |

Research notes from building the provider integrations:
- [Claude Code protocol](docs/class-a-claude.md)
- [Codex protocol](docs/class-a-codex.md)
- Verification runs: [phase 1](docs/verify-phase1.md), [phase 3](docs/verify-phase3.md)

## Layout

```
cmd/intact            the binary: flags, TOTP enrollment, listener
internal/httpapi      routes, /v1 routing and failover, dashboard API, MCP, OAuth login, quota, model tests
internal/translate    OpenAI ⇄ Anthropic, Responses and Gemini request/answer/stream translation
internal/provider     the provider registry (base URL, auth, identity headers, list endpoint)
internal/filter       the blacklist engine
internal/drift        structure learning and change detection
internal/store        SQLite (pure Go): connections, models, filters, keys, drift, usage, settings
internal/oauth        the refresh-token grant
internal/upstream     the outbound HTTP client
internal/web          the dashboard (index.html) and icons, embedded in the binary
```

## Tests

```bash
go test ./...
go vet ./...
```

The tests run against fake upstreams. None of them calls a real provider.
