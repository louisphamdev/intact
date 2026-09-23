# API reference

## Authentication

| Gate | How |
| --- | --- |
| **token** | An API key or `INTACT_API_TOKEN`, as `Authorization: Bearer <key>` or `x-api-key: <key>` |
| **admin** | `INTACT_API_TOKEN` or the dashboard session cookie. An API key gets 403. |
| **session** | The dashboard's session cookie, from the TOTP sign-in |
| **open** | none |

An API key calls the proxy and reads the management API. It does not manage
intact. Every route marked **admin** below needs the master token or the
session. [Security](security.md) lists the same routes and the masked reads.

Errors are JSON: `{"error":{"message":"…"}}`.

## Proxy (token)

| Method and path | Purpose |
| --- | --- |
| `GET /v1/models` | Every model that is on, as `<provider>/<model>`. The format serves both OpenAI (`object`, `owned_by`) and Anthropic (`type`, `display_name`) clients. |
| `POST /v1/chat/completions` | OpenAI shape, to any provider. |
| `POST /v1/messages` | Anthropic shape, to any provider. |
| `POST /v1/messages/count_tokens` | Anthropic token count; estimated when no Anthropic account serves the model. |
| `* /v1/{path}` | Any other path, passed through to the provider at that path (`/v1/responses`, `/v1/systemone`, …). |

Response header: `X-Intact-Model`, the upstream model that answered, when
intact picked it (Antigravity variants). See [Routing](routing.md).

## Management API (token, some admin)

### Providers and accounts

| Method and path | Purpose |
| --- | --- |
| `GET /api/providers` | Providers with their account counts and setup kind. |
| `GET /api/accounts` | Accounts (never credentials): id, provider, label, active, base URL. |
| `POST /api/accounts/{id}/active` (admin) | `{"active":bool}`. |
| `POST /api/accounts/{id}/test` (admin) | `{"model":"…"}` optional. Test one account alone; the result is kept. |

### Models

| Method and path | Purpose |
| --- | --- |
| `GET /api/providers/{id}/models` | The model table (see the fields below). `?refresh=1` fetches the list first. |
| `GET /api/providers/{id}/models/raw` | The provider's last list answer, as it came. |
| `POST /api/providers/{id}/models/active` (admin) | `{"models":[…],"active":bool}`. |
| `POST /api/providers/{id}/models/delete` (admin) | `{"models":[…]}`. A model still listed comes back on the next fetch. |
| `POST /api/providers/{id}/models/test` (admin) | `{"model":"…","account":"<connection id, optional>"}`. Returns `ok`, `status`, `ms`, `message`, `account`, and `active` when the test set the switch. |
| `GET /api/providers/{id}/rotation` | `{"rotation":{mode,sticky,order},"next":"<connection id>","used":n}`. |
| `POST /api/providers/{id}/rotation` (admin) | `{"mode":"round-robin"\|"fallback","sticky":1–1000,"order":[connection ids]}`; fields left out keep their value. |
| `GET /api/providers/{id}/model-policy` | `{"policy":{autoTest,onlyFree,lastRun,lastResult},"running":bool}`. |
| `POST /api/providers/{id}/model-policy` (admin) | `{"autoTest":bool,"onlyFree":bool}`: stores and applies it. |

Fields of the model table:

| Field | Contents |
| --- | --- |
| `models[]` | `model`, `active`, `firstSeen`, `lastSeen`, `testAt`, `testOk`, `testMs`, `testMsg`, `testConn` |
| `ok` | Whether the last fetch succeeded. |
| `fetchedAt` | When the list was last fetched. |
| `policy` | The provider's Auto test / Only free policy. |
| `running` | Whether an auto test is running. |
| `variants` | Folded variants, base → levels, default first. |
| `info` | Per model: `thinking`, `always`, `efforts`, `default`, `paid`. |

### Declared providers

| Method and path | Purpose |
| --- | --- |
| `GET /api/provider-defs` | Every declared provider's definition. For an API key the OAuth client secret and every static header value read `••••`. |
| `GET /api/provider-defs/{id}` | One definition, masked the same way for an API key. |
| `POST /api/provider-defs` (admin) | Declare a provider, or replace its definition (the JSON of [Declared providers](providers.md#declared-providers)). |
| `DELETE /api/provider-defs/{id}` (admin) | Delete a declared provider with no account left. |

Declared OAuth providers sign in through `POST /oauth/{id}/start` and then:
- `POST /oauth/{id}/finish` for the browser flow;
- `POST /oauth/{id}/poll` for the device flow, as `{"deviceCode":"…"}`.

### Rankings

| Method and path | Purpose |
| --- | --- |
| `GET /api/rankings` | The LMArena boards intact holds (`?q=` filters names), with `published`, `fetchedAt` and each board's top rating. |
| `POST /api/rankings/refresh` (admin) | Read the boards now (otherwise once a day). |
| `POST /api/rankings/alias` (admin) | `{"provider","model","name"}`: map a model to a board name by hand; `""` returns to automatic matching, `"-"` marks it as not on the board. |

The model table also carries `arena` (per model: the matched entry, `how` and
its `scores` per board: `rating`, `rank`, `votes`, `tier`) and `arenaMeta`.

### Blacklist

| Method and path | Purpose |
| --- | --- |
| `GET /api/filters` | Every rule. |
| `POST /api/filters` (admin) | `{"provider","kind","pattern","note","enabled"}`; with `id`, updates that rule. |
| `DELETE /api/filters/{id}` (or `POST …/delete`) (admin) | Delete a rule. |

### Drift

| Method and path | Purpose |
| --- | --- |
| `GET /api/drift/changes` | `?provider=&direction=request\|response&unacked=1&since=<id>&limit=` |
| `POST /api/drift/ack` (admin) | `{"ids":[…]}`; no ids acknowledges every change. The body must be JSON and less than 64 KiB, or the answer is 400. |
| `GET /api/drift/fields` | `?provider=&direction=&endpoint=`: learned paths, types and counts. |
| `GET /api/drift/review` | The automatic review: `enabled`, `decisionModel`, `resolverModel`, `ackConfidence`, `ready` (an account serves the decision model), `lastError`. |
| `POST /api/drift/review` (admin) | Any of `{"enabled","decisionModel","resolverModel","ackConfidence"}`; `enabled` needs a decision model. `{"run":true}` judges the waiting changes now. |
| `POST /api/drift/seed` (admin) | `{"direction","provider","endpoint","sse":bool,"documents":[…]}`: learn reference captures, recording no change. |

### Errors

| Route | Meaning |
| --- | --- |
| `GET /api/errors` | `?provider=&class=&status=&signature=&since=<RFC 3339>&limit=`: provider errors, newest first, without bodies. |
| `GET /api/errors/{id}` | One error with its headers, answer and request. An API key reads the stored bodies only for the errors its own requests caused. |
| `GET /api/errors/review`, `POST /api/errors/review` (admin for POST) | The error review: `{"enabled","model","minErrors","replay"}`; `{"run":true}` judges the groups due now. |
| `GET /api/errors/verdicts` | The review's verdicts, newest first. |
| `GET /api/errors/stats` | `?provider=&since=`: errors grouped by signature, with count, classes, models, median latency, first and last time. |

### Alerts

| Route | Meaning |
| --- | --- |
| `GET /api/notify` (admin) | Channels (secrets masked), channel types and their fields, events. |
| `POST /api/notify/channels`, `PUT /api/notify/channels/{id}` (admin) | Create or replace `{"name","type","enabled","events","config"}`. |
| `DELETE /api/notify/channels/{id}` (admin) | Delete. |
| `POST /api/notify/channels/{id}/test` (admin) | Send a test alert. |

### Quota and usage

| Method and path | Purpose |
| --- | --- |
| `GET /api/quota` | `?provider=&connection=`: every active account's quota. Machine callers cannot bypass the cache; `refresh=1` is ignored. |
| `POST /quota/{id}/reset` (session) | `{"resetId":"…","requestId":"…"}`: claim one manual quota reset for an account. |
| `GET /api/usage` | Daily token totals per account and model. |

## MCP (token)

`POST /mcp` is an MCP server over Streamable HTTP (JSON-RPC). `GET /mcp` answers
405.

```bash
claude mcp add --transport http intact https://intact.example/mcp \
  --header "Authorization: Bearer $INTACT_KEY"
```

| Tool | Purpose |
| --- | --- |
| `list_providers` | Providers and their account counts. |
| `list_accounts` | Accounts (no credentials), optionally for one provider. |
| `set_account_active` | Turn an account on or off. |
| `test_account` | Test one account (TypeSafe gets its System One test). |
| `list_models` | Callable models as `<provider>/<model>`. |
| `list_provider_models` | A provider's model table. |
| `set_models_active` | Switch models on or off. |
| `set_model_policy` | Auto test and Only free for a provider. |
| `list_provider_defs`, `put_provider_def`, `delete_provider_def` | Declared providers. |
| `get_model_rankings` | LMArena rating, rank and tier of a provider's models on each board. |
| `list_filters`, `add_filter`, `update_filter`, `delete_filter` | The blacklist. |
| `list_drift_changes`, `ack_drift_changes`, `list_drift_fields` | Drift. Changes carry `client`, `verdict`, `verdictConf`, `verdictBy`, `verdictNote`, `resolved`, `autoAcked`. |
| `list_errors`, `get_error`, `error_stats` | The [error log](errors.md). |
| `error_review`, `list_error_verdicts` | The [error review](errors.md#automatic-review). |
| `list_notify_channels`, `put_notify_channel`, `test_notify_channel`, `delete_notify_channel` | [Alerts](alerts.md). |
| `drift_review` | The automatic review: state, models, switch, run now. |
| `get_quota` | Quota of every active account. |
| `get_usage` | Daily token totals, optionally for one day. |

## Dashboard routes (session)

The dashboard calls these routes. They mirror the management API.

| Method and path | Purpose |
| --- | --- |
| `GET /` | The dashboard. |
| `GET /accounts`, `POST /accounts` | List. Add, with a form body: `provider`, `label`, `secret`, `account_id`, `base_url`, `api`. |
| `POST /accounts/{id}/delete`, `/active`, `/label`, `/test` | Account actions. |
| `GET /account-tests` | The last test of every account. |
| `GET /accounts/{id}/models` | One account's model list. |
| `GET /providers` | Providers with setup kind, auth kind, icon. |
| `GET /providers/{id}/models`, `/model-table`, `/model-policy` | As the API. |
| `POST /providers/{id}/models/active`, `/delete`, `/test`, `/model-policy` | As the API. |
| `POST /oauth/{provider}/start` | `{"label":"…"}`. Returns the authorize URL and state, or a device code for GitHub. |
| `POST /oauth/{provider}/finish` | `{"state":"…","input":"<redirect URL, code#state or code>","label":"…"}`. |
| `POST /oauth/github/poll` | Poll the device flow. |
| `GET/POST /keys`, `POST /keys/{id}/reveal`, `/active`, `/delete` | API keys. |
| `GET/POST /filters`, `POST /filters/{id}/delete` | The blacklist. |
| `GET /drift/changes`, `POST /drift/ack`, `GET /drift/fields` | Drift. |
| `GET /errors`, `GET /errors/stats`, `GET /errors/{id}`, `/errors/review`, `/errors/verdicts` | Errors. |
| `/notify`, `/notify/channels…` | Alerts. |
| `GET /quota`, `GET /usage` | Quota and usage. |
| `GET /rankings`, `POST /rankings/refresh`, `POST /rankings/alias` | Rankings, as the API. |
| `GET/POST /provider-defs`, `GET /provider-defs/{id}`, `POST /provider-defs/{id}/delete` | Declared providers, as the API. |
| `POST /oauth/{provider}/poll` | Poll a declared provider's device sign-in. |
| `GET/POST /ui-settings/{key}` | Dashboard preferences stored on the server (`quota-view`). |

## Open routes

| Method and path | Purpose |
| --- | --- |
| `GET /login`, `POST /login`, `POST /logout` | The TOTP sign-in. |
| `GET /icons/{name}` | Provider icons. |
| `GET /favicon.svg`, `/favicon.ico`, `/apple-touch-icon.png` | Site icons. |
