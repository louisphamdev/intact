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

## Automatic review

Each recorded change can be settled with no person involved, by two models
set in **Drift → ⚙ Review settings**, `POST /api/drift/review` or the MCP tool
`drift_review`:

| Setting | Model | Role |
| --- | --- | --- |
| **Decision model** | a System One model, such as `typesafe/jev-latest` | Chooses each change's cause, with a calibrated confidence. **Required**: the *Auto-resolve drift* switch stays off without it. |
| **Resolver model** | any chat model intact serves, such as `antigravity/gemini-3.8-flash` | Settles every change the decision model does not close. Empty: those wait for a person. |
| **Confidence to act alone** | | 60% by default. |

1. intact computes what it knows of the change:
   - whether the path lies in tool call arguments or in a tool's schema;
   - whether the same field was reported removed and returned today;
   - whether the answer was streamed;
   - whether a client started streaming or sending tools that hour;
   - which client sent it, and whether that client is new;
   - how many other changes the provider had that hour;
   - how many requests the provider refused since, from the
     [error log](errors.md): `rejected` answers and fake 429s. Real limits,
     timeouts and network errors are not counted.
2. The decision model chooses the cause:

   | Cause | Meaning |
   | --- | --- |
   | `new_client_usage` | a client started using a feature, or a new client began; the API did not change |
   | `data_noise` | the path lies in user data or tool arguments |
   | `optional_field_flap` | an optional field some documents carry and others do not |
   | `provider_format_change` | the provider changed its answer, or began requiring or rejecting something |

3. A benign cause (the first three) with the confidence to act alone, and no
   failed answer since, **closes the change**. It is marked
   *auto-acknowledged*.
4. Every other change goes to the **resolver**: low confidence, a likely
   provider change, or failed answers. The resolver receives:
   - the facts;
   - the decision model's leaning and probabilities.

   Its action is final:
   - **acknowledge** closes the change with its reason;
   - **blacklist** adds a field rule for that provider and closes the change.

   **Guard on blacklisting.** A model may blacklist only when all of these
   hold, and otherwise the change is only acknowledged, with the reason noted:
   - it is a request field that appeared or changed type;
   - the provider has refused requests since the change;
   - it is not a field the request needs (`model`, `messages`, `tools`,
     `stream`, `system`, `max_tokens`…).
5. Without a resolver, the changes the decision model did not close wait in
   **New**, with the verdict and why.

Each verdict shows which model gave it, its confidence, and the resolver's
reason.

Operation:
- **When it runs:** the review runs every minute. It also runs at once from
  **Review now**.
- **Errors:** when a model cannot be reached, it pauses ten minutes and says
  so on the page.
- **Back-review:** changes judged before a resolver was set are handed to it
  once one is.

### Why a decision model, and why only the cause (decision, 2026-09-22)

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
- **Blacklisting needs evidence.** It is left to the resolver and gated by
  failed answers, never asked of the decision model.
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

### Hands-off (decision, 2026-09-22)

The owner asked for drift to need no person once the review is set up. So the
roles split:
- the decision model closes what it is sure of, which is cheap and fast;
- the resolver, a general chat model, settles the rest with a final action
  and a written reason.

Both models are configured, not built in, so another deployment picks its
own, and the review cannot be switched on without a decision model. On
intact's own deployment:
- decision model: `typesafe/jev-latest`;
- resolver: `antigravity/gemini-3.8-flash`.
