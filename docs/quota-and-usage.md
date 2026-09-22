# Quota and usage

## Quota

The **Quota** page shows every active account whose quota can be read.

Each account's quota is read from the provider's own endpoint where one exists:

| Provider | Source | Windows |
| --- | --- | --- |
| Codex | `wham/usage` | rolling windows (5h, 7d) with reset times |
| Claude Code | `api/oauth/usage` | 5h and 7d windows, per-model weekly pools |
| GitHub Copilot | `copilot_internal/user` | monthly premium requests, chat, completions |
| Antigravity | quota summary + `fetchAvailableModels` | per-model share of the model's bucket, with its reset |
| Groq | the rate-limit headers of a `/models` call | requests and tokens per window |
| OpenRouter | `/api/v1/key` | credit used against the key's limit |

A provider without a quota endpoint falls back to the rate-limit headers of its
last answer. intact keeps those headers passively from every response:
- `x-ratelimit-*`, `ratelimit-*`, `anthropic-ratelimit-*`;
- `x-codex-*`, `x-quota-snapshot-*`;
- `retry-after`.

On the page:
- **Quotas are cached for 60 seconds.** **Refresh** reads them again.
- **Accounts with nothing readable are hidden.** An unreadable quota is not
  shown as an empty card.
- **Each card is titled by provider**, with the account name below it.
- **Edit shows or hides windows.** A provider can have many windows, and Edit
  lets you tick the ones to show for each provider. The choice is stored on the
  server (the `quota-view` setting).

API: `GET /api/quota?provider=&refresh=1`. MCP: `get_quota`.

## Usage

intact reads the `usage` object of every answer: JSON or a stream,
compressed or not, including cache tokens. It adds the counts to a daily total
per account and model. It reads the answer and never changes it:
- For a large answer, only the head and tail are read, which bounds memory.
- Upstreams are asked for `Accept-Encoding: identity`, so the answer can always
  be parsed.

The **Usage** page shows the daily totals. API: `GET /api/usage`. MCP:
`get_usage` (optionally one day, `YYYY-MM-DD`).
