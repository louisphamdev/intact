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
  GitHub Copilot). Any other provider is declared from the dashboard without
  code: API key, browser OAuth or device-code OAuth, with the fields each kind
  needs, or pasted as JSON. [Providers](docs/providers.md)
- **Model management.** A table per provider shows each model with an on/off
  switch, its thinking support and effort levels, and its last test.
  - Lists are fetched every hour.
  - Auto test keeps on only the models that answer.
  - Only free keeps on only the free models.
  - Antigravity's per-level models are folded into one model, picked by effort.
  - Each model shows its LMArena rating, rank and tier (S–D) on the Overall,
    Coding and WebDev boards, so a strong model stands out from a weak one.

  [Models](docs/models.md)
- **Blacklist.** Editable rules remove what a provider refuses (a body field, a
  tool-schema key, a system-prompt line, a header) before the request leaves.
  [Blacklist](docs/blacklist.md)
- **Drift alerts.** For providers reached as a coding tool, intact learns the
  structure of requests (per client) and answers, and records when a field
  appears, disappears or changes type.
  - **Auto-resolve drift** settles every change with no person involved:
    - a configured decision model (such as TypeSafe's Jev) closes the benign
      ones it is sure of;
    - a configured resolver chat model (such as Gemini 3.8 Flash) settles
      the rest, acknowledging or blacklisting a field a provider refuses.
  - The switch needs a decision model.

  The decisions and the trial behind them are in
  [Drift](docs/drift.md#automatic-review).
- **Contract Lab.** Evaluates converter loss and schema drift across client
  tools and providers without storing prompt or answer text. Evaluates shapes,
  detects changes, and generates test fixtures.
  [Contracts](docs/contracts.md)
- **Error log.** Every error a provider answers is kept for 14 days, with the
  request that was sent, the answer, the headers and the latency. That
  includes the attempts intact failed over from.
  - Errors are grouped by signature.
  - A 429 is marked a real limit or a fake one: a fake one comes back at once
    while the account's quota is left.

  - **Auto-fix errors**: a configured chat model proposes a fix for each
    recurring group (blacklist what the provider refuses, switch off a
    dropped model or a revoked account). intact replays the failing request
    to prove a rule before keeping it.

  [Errors](docs/errors.md)
- **Alerts.** intact tells the channels you declare what it fixed on its own
  and what it could not fix: a Telegram chat or topic, or a webhook.
  [Alerts](docs/alerts.md)
- **Quota and usage.** Quota is read from each provider (rolling windows, resets, weekly
  pools, per-model shares). Manual resets can be claimed from the dashboard. Token usage is counted per account, model and day.
  [Quota and usage](docs/quota-and-usage.md)
- **Management API and MCP.** Every dashboard action has a route under `/api/*`,
  and a tool on the MCP server at `/mcp`. The routes that change intact need the
  dashboard session or the master token. A dashboard key calls `/v1` and reads
  what is not admin-only. [API](docs/api.md)
- **Dashboard.** A single embedded page with sign-in by a TOTP code and nothing
  else. No password. API keys are created there. [Security](docs/security.md)
  - It is in English and Vietnamese.
  - It has light, dark and automatic (system) themes.
  - Both choices are made at the bottom of the menu and kept in the browser.

## Quick start

```bash
npm install -g intact-proxy           # or: CGO_ENABLED=0 go build -o intact ./cmd/intact
intact -db ./intact.db -addr 127.0.0.1:20142
```

On its first start, intact makes a new TOTP secret for this install and prints
it with the steps to add it to an authenticator app. Each install gets its own
secret, kept in its database. To print the steps again, run
`intact -db ./intact.db -show-totp`.

Open the dashboard, sign in with the code from the app, add an account under **Providers**,
create an API key under **Endpoint → API keys**, then:

```bash
curl http://127.0.0.1:20142/v1/chat/completions \
  -H "Authorization: Bearer $INTACT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"groq/llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}'
```

[Getting started](docs/getting-started.md) covers configuration and a systemd
and Cloudflare Tunnel deployment.

## Self-improvement with llm-switcher

[llm-switcher](https://github.com/louisphamdev/llm-switcher) runs on each
developer machine. It lets Claude Code and Codex use intact as their provider,
and it converts each request to the format of the target model. The two tools
find and correct their own faults, in two loops.

- **intact corrects what providers refuse.** Drift learns the structure of
  requests. The error review groups recurring errors. For a fake 429, intact
  replays the failing request and removes the system prompt text in halves. It
  keeps the smallest text that the provider refuses as a filter in its database.
  All clients get the fix at once, with no client update. Two examples: the
  sentence "You are Codex, an agent based on GPT-5" and the sentence
  "You are a Claude agent, built on Anthropic's Claude Agent SDK". Antigravity
  answered both with a fake 429.
- **llm-switcher corrects what its converter loses.** When the contract lab is
  on, llm-switcher sends a sample of complete exchanges to intact. It masks the
  client content on the machine first. intact compares the two halves of each
  exchange and records each field that the conversion lost.
  `switch contract-check` then writes one failing test for each finding, and
  the fix goes into the converter.

The rules stay in configuration, not in code. A person reads the alerts and
the verdicts, and does not find each fault by hand.
[Errors](docs/errors.md) · [Contracts](docs/contracts.md)

## Documentation

| Page | Contents |
| --- | --- |
| [Getting started](docs/getting-started.md) | Build, flags, environment, first account, deployment |
| [Routing](docs/routing.md) | Model resolution, translation, rotation, failover, variants |
| [Providers](docs/providers.md) | Every built-in provider, sign-in flows, declaring a provider |
| [Models](docs/models.md) | The model table, fetching, switches, tests, auto test, only free, thinking |
| [Blacklist](docs/blacklist.md) | Request filters: kinds, scope, seeded rules |
| [Drift](docs/drift.md) | Structure learning and change alerts |
| [Errors](docs/errors.md) | The error log: what is kept, classes, fake 429s, the automatic review |
| [Alerts](docs/alerts.md) | Alert channels (Telegram, webhook) and events |
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

## License

intact is released under the MIT License (SPDX identifier `MIT`). The full text
is in [LICENSE](LICENSE).

To report a security fault, read [SECURITY.md](SECURITY.md).
