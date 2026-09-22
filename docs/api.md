# API reference

## Authentication

| Gate | How |
| --- | --- |
| **token** | An API key or `INTACT_API_TOKEN`, as `Authorization: Bearer <key>` or `x-api-key: <key>` |
| **session** | The dashboard's session cookie, from the TOTP sign-in |
| **open** | none |

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

## Management API (token)

### Providers and accounts

| Method and path | Purpose |
| --- | --- |
| `GET /api/providers` | Providers with their account counts and setup kind. |
| `GET /api/accounts` | Accounts (never credentials): id, provider, label, active, base URL. |
| `POST /api/accounts/{id}/active` | `{"active":bool}`. |
| `POST /api/accounts/{id}/test` | `{"model":"…"}` optional. Test one account alone; the result is kept. |

### Models

| Method and path | Purpose |
| --- | --- |
| `GET /api/providers/{id}/models` | The model table (see the fields below). `?refresh=1` fetches the list first. |
| `GET /api/providers/{id}/models/raw` | The provider's last list answer, as it came. |
| `POST /api/providers/{id}/models/active` | `{"models":[…],"active":bool}`. |
| `POST /api/providers/{id}/models/delete` | `{"models":[…]}`. A model still listed comes back on the next fetch. |
| `POST /api/providers/{id}/models/test` | `{"model":"…","account":"<connection id, optional>"}`. Returns `ok`, `status`, `ms`, `message`, `account`, and `active` when the test set the switch. |
| `GET /api/providers/{id}/model-policy` | `{"policy":{autoTest,onlyFree,lastRun,lastResult},"running":bool}`. |
| `POST /api/providers/{id}/model-policy` | `{"autoTest":bool,"onlyFree":bool}`: stores and applies it. |

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

### Rankings

| Method and path | Purpose |
| --- | --- |
| `GET /api/rankings` | The LMArena boards intact holds (`?q=` filters names), with `published`, `fetchedAt` and each board's top rating. |
| `POST /api/rankings/refresh` | Read the boards now (otherwise once a day). |
| `POST /api/rankings/alias` | `{"provider","model","name"}`: map a model to a board name by hand; `""` returns to automatic matching, `"-"` marks it as not on the board. |

The model table also carries `arena` (per model: the matched entry, `how` and
its `scores` per board: `rating`, `rank`, `votes`, `tier`) and `arenaMeta`.

### Blacklist

| Method and path | Purpose |
| --- | --- |
| `GET /api/filters` | Every rule. |
| `POST /api/filters` | `{"provider","kind","pattern","note","enabled"}`; with `id`, updates that rule. |
| `DELETE /api/filters/{id}` (or `POST …/delete`) | Delete a rule. |

### Drift

| Method and path | Purpose |
| --- | --- |
| `GET /api/drift/changes` | `?provider=&direction=request\|response&unacked=1&since=<id>&limit=` |
| `POST /api/drift/ack` | `{"ids":[…]}`; no ids acknowledges every change. |
| `GET /api/drift/fields` | `?provider=&direction=&endpoint=`: learned paths, types and counts. |
| `POST /api/drift/seed` | `{"direction","provider","endpoint","sse":bool,"documents":[…]}`: learn reference captures, recording no change. |

### Quota and usage

| Method and path | Purpose |
| --- | --- |
| `GET /api/quota` | `?provider=&refresh=1`: every active account's quota. |
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
| `get_model_rankings` | LMArena rating, rank and tier of a provider's models on each board. |
| `list_filters`, `add_filter`, `update_filter`, `delete_filter` | The blacklist. |
| `list_drift_changes`, `ack_drift_changes`, `list_drift_fields` | Drift. |
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
| `GET /quota`, `GET /usage` | Quota and usage. |
| `GET /rankings`, `POST /rankings/refresh`, `POST /rankings/alias` | Rankings, as the API. |
| `GET/POST /ui-settings/{key}` | Dashboard preferences stored on the server (`quota-view`). |

## Open routes

| Method and path | Purpose |
| --- | --- |
| `GET /login`, `POST /login`, `POST /logout` | The TOTP sign-in. |
| `GET /icons/{name}` | Provider icons. |
| `GET /favicon.svg`, `/favicon.ico`, `/apple-touch-icon.png` | Site icons. |
