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

Some object keys are names rather than fields, and they collapse to `{*}` so
they do not raise alerts:
- keys under `properties` (tool parameters) and `answers`;
- id-like keys;
- objects wider than 96 keys.

Some values are data, not API: `args`, `arguments`, `metadata`,
`client_metadata`, `extra_body` and `headers`. For these, only each value's
type is learned; nothing inside them is. A tool schema's `properties` is still
learned in depth, because a new schema keyword there is what a provider may
refuse.

**Requests are learned per client.** The client is the API key's name,
`env` for the environment token, or `internal` for intact's own calls (model
tests, reviews). One client's habits do not read as another's change: a client
that always sends `max_tokens` next to one that never does is not reported as
`max_tokens` coming and going. Answers are learned per provider.

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

## Review by Jev

Each recorded change is judged by a decision model: TypeSafe's **Jev**,
through intact's own `/v1/systemone` and the TypeSafe account.
1. intact computes what it knows of the change:
   - whether the path lies in tool call arguments or in a tool's schema;
   - whether the same field was reported removed and returned today;
   - whether the answer was streamed;
   - whether a client started streaming or sending tools that hour;
   - which client sent it, and whether that client is new;
   - how many other changes the provider had that hour;
   - how many answers of the provider failed since.
2. Jev chooses the cause:

   | Cause | Meaning |
   | --- | --- |
   | `new_client_usage` | a client started using a feature, or a new client began; the API did not change |
   | `data_noise` | the path lies in user data or tool arguments |
   | `optional_field_flap` | an optional field some documents carry and others do not |
   | `provider_format_change` | the provider changed its answer, or began requiring or rejecting something |

3. A benign cause (the first three) with confidence of at least **60%**, and
   no failed answer since, **acknowledges the change on its own**. It is
   marked *auto-acknowledged*.
4. Anything else (a provider change, low confidence, failures) stays in
   **New**, with the verdict beside it for a person to decide.

Operation:
- **When it runs:** the review runs every minute. It also runs at once from
  **Review now** on the Drift page, `POST /api/drift/review {"run":true}`, or
  the MCP tool `drift_review`.
- **Switch:** the switch on the Drift page turns it on or off.
- **Errors:** when Jev cannot be reached, the review pauses ten minutes and
  says so on the page.

### Why Jev, and why only the cause (decision, 2026-09-22)

The first 48 changes recorded in production came from Hermes starting to call
through intact: streaming, tools, JSON schema keywords. A hand review found
none that was a provider change. The same 48 were given to Jev:

| Trial | Matched the hand review |
| --- | --- |
| Change only (path, types, sample) | 33 of 48; **19 of 21 when Jev's confidence was ≥ 0.6** |
| With the facts above | **44 of 48**; the 4 others split between two benign causes |

What the trial showed:
- **Confidence tells apart what Jev is sure of.** Hence the 60% bar for acting
  alone.
- **Facts matter more than wording.** Jev decides well on what it is told, and
  intact knows the facts for sure.
- **Only the cause is asked.** Yes/no questions ("should this be blacklisted?",
  "does a person need to see it?") came back between 0.15 and 0.75 for every
  change and told nothing apart.
- **Blacklisting stays a person's decision,** helped by the failed-answer
  count.
- **Cost is small.** A judgement is one call of about 700 tokens and takes a
  few tenths of a second, and changes are rare once a client's structure is
  learned.

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
