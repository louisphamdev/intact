# Errors

intact keeps every error a provider answers, so a failure can be studied
after the fact instead of only read in a log line. Each attempt is kept,
including the attempts that intact failed over from, even when the client got
an answer from the next account.

## What is kept

| Field | Meaning |
| --- | --- |
| time, provider, account, model | Where the attempt went. The model is the provider's own id, such as `gemini-3.8-flash-high`. |
| client | The API key's name, `env` for the environment token, or `internal` for intact's own calls (model tests, reviews). |
| endpoint | The provider path that was called. |
| status | The HTTP status, or none when the call itself failed (network, timeout). |
| latency | Time until the provider answered. |
| headers | `Retry-After`, `Content-Type`, the request id headers, and every rate-limit or quota header. |
| answer | The body the provider sent back, up to 64 KB. |
| request | The exact body that was sent, after the blacklist ran, up to 64 KB. |
| signature | The status and the message with ids and numbers removed, so the same error on another request falls in the same group. |
| class | A first reading of the error (below). |
| quota left | For a 429: the share of the account's quota left for that model, read right after the error. |

The client still gets the provider's answer untouched: the body is read, kept,
and handed on as it came.

A call the client dropped is not kept, because it is not the provider's error.

## Classes

| Class | When |
| --- | --- |
| `network` | No answer: the connection failed. |
| `timeout` | No answer before the deadline, or 408, 504 or 524. |
| `auth` | 401 or 403. |
| `rejected` | Any other 4xx: the provider refused the request. |
| `rate_limit` | A 429 while the quota is low or unknown: a real limit. |
| `fake_rate_limit` | A 429 answered in under 800 ms while more than 10% of the model's quota is left. |
| `server` | 5xx. |

### Fake 429 (decision, 2026-09-22)

Antigravity answers some requests it refuses (an unknown field, or a line in
the system prompt it does not accept) with the same body as a real rate limit:

```json
{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}
```

The body cannot tell the two apart. Two measures can:
- **Latency.** Measured on production:
  - a refused request came back in about 0.2 s;
  - a real limit came back after 2 s or more.
- **Quota.** Right after the refused request, `fetchAvailableModels` still
  showed 96% of the model's quota left.

So after each 429, intact reads the account's quota in the background, then
marks the error as a real limit or as `fake_rate_limit`. A quota read is
cached for a while, so a burst of 429s does not become a burst of quota calls.

A fake 429 is a refusal. It counts as one for the [drift review](drift.md#automatic-review),
so a field that started such refusals can be blacklisted.

## Keeping

- Errors are kept 14 days.
- At most 20 000 errors are kept; the oldest go first.
- Pruning runs every 200 new errors.

## Reading errors

- **Dashboard.** The **Errors** page lets you pick 24 hours, 7 days or
  everything kept, and has two tabs:
  - **Groups**: one row per provider and signature, with the count, the
    classes, the models, the median latency, and the first and last time.
  - **All**: one row per error.

  Every column sorts and filters. A row opens the error in full: headers,
  answer and request.
- **API.**
  - `GET /api/errors?provider=&class=&status=&signature=&since=&limit=` lists
    errors, newest first, without the bodies.
  - `GET /api/errors/{id}` returns one error with its headers, answer and
    request.
  - `GET /api/errors/stats?provider=&since=` groups errors by signature.
  - `since` is an RFC 3339 time.
- **MCP.** The tools are `list_errors`, `get_error` and `error_stats`. An
  agent can start from `error_stats`, then open the newest error of a group
  with `get_error`.
