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
| A declared provider with `quotaUrl` | its `quotaUrl` | the windows that the URL gives |

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

API: `GET /api/quota?provider=&connection=`. MCP: `get_quota`.

### Quota URL of a declared provider

A [declared provider](providers.md#declared-providers) can set `quotaUrl`.
intact sends `GET <quotaUrl>` with the account's credential, the auth header
and prefix, and the extra headers of the definition. `{accountId}` in
`quotaUrl` is filled with the account id. It is allowed only when the base URL
also holds `{accountId}`, because only then does an account store its id. The
answer must be JSON in this shape:

```json
{"plan": "Free",
 "windows": [{"name": "Bonus Pack", "usedPct": 4, "used": "10", "limit": "250",
              "resetAt": "2026-11-01T06:02:06Z"}]}
```

| Field | Meaning |
| --- | --- |
| `plan` | Optional. The plan name on the card. |
| `windows[].name` | Required. A window without a name is dropped. |
| `windows[].usedPct` | The used share, 0 to 100. A larger value shows as 100. When it is absent or negative, the card shows `used / limit` with no bar. |
| `windows[].used`, `windows[].limit` | Optional. Text or a number. |
| `windows[].resetAt` | Optional. RFC 3339 text, or epoch seconds or milliseconds. Any other value is dropped. |
| `windows[].unlimited` | Optional. `true` shows the window as unlimited. |

If the URL answers with a status of 300 or more, the card shows the error.
If the rate-limit headers of a recent answer give windows, the card shows
those windows instead, with the **last answer** badge.

### Resets

Some accounts include manual resets:
- **Claude:** weekly resets (`juniper_tide`) and promotional grants (`cedar_ember`).
- **Codex:** rate-limit reset credits.

intact never claims a reset automatically. You must claim each reset manually from the dashboard.
Each reset row shows:
- The title, kind, and available units (`left / total`).
- The rate-limit windows that the reset refills.
- Validity dates, expiration, or next available time.
- Current status and blocked reason if unavailable.

The dashboard hides a reset that has no use left: a used or expired reset, or a grant or credit with `left` 0.
A weekly reset that waits for the 5-hour limit stays visible.
A reset that you claimed on the open page also stays visible, so you can read the result.

To claim a reset, click **Claim** on its row in the dashboard and confirm the dialog.
A claim spends a limited resource and cannot be undone.

API: `POST /quota/{connectionId}/reset` (requires a dashboard session). Machine tokens and MCP callers cannot claim resets.

## Usage

intact reads the `usage` object of every answer: JSON or a stream,
compressed or not, in the Chat, Messages, Responses and Gemini shapes. For
Gemini it reads `usageMetadata`. It adds the
counts to a daily total per account and model, and per API key. It reads the
answer and never changes it:
- For a large answer, only the head and tail are read, which bounds memory.
- Upstreams are asked for `Accept-Encoding: identity`, so the answer can always
  be parsed.

The **Usage** page shows the daily totals. API: `GET /api/usage`. MCP:
`get_usage` (optionally one day, `YYYY-MM-DD`).

Each total has three token counts:

| Field | What it counts |
| --- | --- |
| `inputTokens` | Every prompt token, cached or not. Anthropic cache reads and cache writes are included. |
| `cachedTokens` | The part of `inputTokens` that the provider read from its cache: `cache_read_input_tokens` (Anthropic), `cached_tokens` (OpenAI Chat and Responses), `cachedContentTokenCount` (Gemini). |
| `outputTokens` | Every answer token, reasoning included. For Gemini this is `candidatesTokenCount` plus `thoughtsTokenCount`. |

The **Cached tokens** tile shows the count and its share of the input tokens.
That share tells how well [conversation pinning](routing.md) keeps each
conversation on the account that holds its cache. Totals from before
2026-10-02 have no cached count.
