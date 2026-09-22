# Drift

Providers reached as a coding tool (Claude Code, Codex, Antigravity, Copilot)
change their JSON without notice. Clients also add fields that such providers
then refuse. Drift watches both sides for these providers and records every
structure change, so you can blacklist a field before it breaks traffic, or
notice that a provider changed its answer.

Providers with a public, stable API (Groq, OpenRouter, …) are not watched.

## What is learned

For each **direction × provider × endpoint × event**, intact learns the field
paths of the JSON and the type found at each path. For example:
- `response|codex|responses|response.output_item.done`
- `messages[].content[].type → string`

The four parts are:
- the direction: `request` is what a client sent, `response` is what the
  provider answered;
- the provider;
- the endpoint;
- the event: for a streamed answer, each server-sent event is learned under its
  own name.

Some object keys are names rather than fields, for example tool parameters,
metadata keys and ids. They collapse to `{*}`, so they do not raise alerts.
This applies to keys under `properties`, `metadata`, `arguments`…, to id-like
keys, and to objects wider than 96 keys.

## What is recorded

| Kind | Meaning |
| --- | --- |
| `added` | A path never seen before. |
| `removed` | A path that was nearly always there has been missing for 20 observations. |
| `type` | A path now carries a type it never had. The type set grows, such as `string` → `number\|string`. |
| `returned` | A path marked removed appeared again. |

Some changes are not recorded:
- **The first 3 observations of a key are learning only.** Nothing is recorded
  for them.
- **Only the topmost change is recorded** when a whole object appears or
  disappears, not one change per field inside it.

Each change keeps the start of the document it was seen in.

Observation never slows a request. When the queue is full, a document is
skipped. Counters are written to the database every 30 seconds.

## Seeding

Captures of the genuine tool, taken before intact serves it, can be loaded as
the reference structure. Nothing is recorded for them:

```bash
curl https://intact.example/api/drift/seed -H "Authorization: Bearer $INTACT_KEY" \
  -d '{"direction":"response","provider":"codex","endpoint":"responses","sse":true,"documents":["<captured stream>"]}'
```

## Reading changes

- **Dashboard**: **Drift** lists the changes, newest first. Ack marks them as
  reviewed.
- **API**:
  - `GET /api/drift/changes?provider=&direction=&unacked=1&since=<id>&limit=`
  - `POST /api/drift/ack {"ids":[…]}` (empty `ids` acknowledges all)
  - `GET /api/drift/fields?provider=&direction=&endpoint=`
- **MCP**: `list_drift_changes`, `ack_drift_changes`, `list_drift_fields`.

A typical loop for an agent:
1. List the unacked request changes.
2. Blacklist the fields a provider will refuse (`add_filter`).
3. Acknowledge the changes.
