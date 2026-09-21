# Phase 3 verification — usage counters

Date: 2026-09-21
Scope: the part of Phase 3 that needs no captured traffic.

## What Phase 3 splits into

Phase 3 in the design is "quota endpoints, lazy poll, daily counters, dashboard
numbers". It splits by evidence:

- **Unblocked.** The daily usage counter, the tap that fills it, the `/usage`
  endpoint, and the dashboard table. These read the `usage` object that already
  flows through the proxy. The object shape is documented (OpenAI
  `prompt_tokens`/`completion_tokens`, Anthropic `input_tokens`/`output_tokens`).
- **Blocked.** The per-provider quota endpoint parser (for example claude
  `api/oauth/usage`). The response shape is not captured, and the design forbids a
  guess. This waits behind the same capture gate as the class A providers of
  Phase 4.

This file records the unblocked part.

## What was built

| Unit | File |
| --- | --- |
| Daily counter store, keyed by day, account and model | `internal/store/usage.go` |
| Usage parser for plain JSON and SSE, both vocabularies | `internal/usage/parse.go` |
| Response tap that fills the counter without altering the bytes | `internal/httpapi/proxy.go` |
| `GET /usage` endpoint | `internal/httpapi/accounts.go`, `internal/httpapi/server.go` |
| Dashboard usage table | `internal/web/index.html` |

## What was proven

### 1. The tap does not change the response

`TestProxyRecordsUsageWithoutAlteringTheResponse` sends a body with a `usage`
object through the proxy. The test asserts that the bytes the caller receives are
byte-identical to the upstream body, and that one usage row is written.

### 2. The counter aggregates per day, account and model

`TestAddUsageAggregatesPerDayAccountModel` folds two calls on the same key into
one row that sums the tokens and counts two requests. A different model is a
separate row.

### 3. The parser reads both providers and both shapes

`internal/usage` tests cover OpenAI plain JSON, Anthropic plain JSON, an Anthropic
SSE stream (input from `message_start`, output from `message_delta`), an OpenAI
SSE stream (usage in the final chunk), and a body with no usage.

### 4. The binary serves the numbers

```
GET /usage      -> {"usage":[]}
GET /            -> the dashboard, with the Usage table
```

Verified against a live binary on `127.0.0.1:20155`.

## Test command

```
CGO_ENABLED=0 go test ./...
```

All packages pass. `go vet ./...` is clean.

## Adversarial review and the fixes it forced

A roundtable review (9 read-only lanes on a free model, then verified by hand
against the code) found six real defects in the first cut. All six are fixed,
each with a test that failed first:

| Defect | Fix |
| --- | --- |
| A gzip response body reached the parser as compressed bytes, so usage was lost for any client that sends `Accept-Encoding: gzip` (the default in the provider SDKs) | The counter decompresses its own copy; the caller still gets the original bytes |
| A plain JSON body that carried the text `data:` was read as an SSE stream and dropped | The parser treats a body that starts with `{` as one JSON object |
| A stream larger than the tap limit lost its usage, which sits in the final chunk | The tap keeps both the head and the tail of the response |
| Anthropic cached prompt tokens (`cache_read_input_tokens`, `cache_creation_input_tokens`) were not counted | They are added to the input total |
| A store write error was dropped in silence | The error is logged |
| The dashboard built rows with `innerHTML`, so a model string could inject script | Every cell is set with `textContent` |

## What is still open

- The quota endpoint parsers wait for captured traffic per provider.
- The lazy-poll policy (once per 15 minutes, and after a 429 or 409) has no code
  yet, because it has no quota endpoint to poll.
- Advisory items from the review, not yet actioned: a body that carries both
  token vocabularies double-counts, a zero-token response is not recorded, an
  error status with a usage body still counts as a request, the `/usage` query
  has no row limit, and the SSE parser runs `json.Unmarshal` on every line.
