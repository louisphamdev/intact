# Routing

Every call goes to one base URL, `/v1`. The `model` field of the body decides
where the call goes. The path after `/v1` decides the request shape.

## Resolving the model

| `model` in the body | Served by |
| --- | --- |
| `groq/llama-3.3-70b-versatile` | The provider named by the prefix. intact removes the prefix from the body (a byte splice; nothing else in the body is touched). |
| `llama-3.3-70b-versatile` | Every provider whose model list has that id. Their active accounts are pooled. |
| `meta-llama/llama-3.3-70b-instruct` | A bare id: the part before the slash is a provider id only when a provider of that id exists. |

The rules for a model:
- A model switched off in the model table is not listed in `/v1/models`, is
  never picked for a bare id, and a call naming it gets `403`.
- An inactive account is skipped.
- `GET /v1/models` lists every model that is on, as `<provider>/<model>`.

## Request shapes and translation

| Path | Client shape |
| --- | --- |
| `/v1/chat/completions` | OpenAI Chat Completions |
| `/v1/messages` | Anthropic Messages |
| `/v1/messages/count_tokens` | Anthropic token count. It is estimated locally when no Anthropic account serves the model. |
| any other path (`/v1/responses`, `/v1/systemone`, …) | Passed through to the provider at the same path |

Each provider speaks one shape: OpenAI, Anthropic, Responses (Codex), or Gemini
inside Antigravity's envelope. When the client's shape differs from the
provider's, intact translates:
- the request, through OpenAI Chat as the common form;
- the answer, whole or as a server-sent-event stream, back into the client's
  shape.

When the two shapes are the same, the bytes pass through untouched, at the
provider's own path.

Details worth knowing:
- **Codex and Antigravity answer only as a stream.** When the client did not
  ask to stream, intact collects the stream and returns one whole answer.
- **Copilot** serves Claude models on `/v1/messages`. It serves some models
  only on `/responses`: intact learns those from Copilot's refusal and sends
  them there.
- **Antigravity** requests are wrapped in its envelope: project, session id,
  and a request id per call. Gemini thought signatures are kept between turns,
  so tool calls survive a round trip.
- **Tool calls, images and reasoning** are carried across shapes where both
  sides support them.

## Rotation and failover

For each call intact builds the list of accounts that serve the model, and
starts at the next account in a per-model rotation. Then, for each account:

1. An OAuth token close to expiry is refreshed before use.
2. The request is sent, after the [blacklist](blacklist.md) has run on the exact
   bytes that will leave.
3. A `401` from an OAuth account forces a token refresh and one retry.
   Copilot's exchanged token is re-exchanged instead.
4. A busy answer (`429`, `500`, `503`, `409`) moves to the next account and
   writes a log line: `connection <id>: <provider> answered 429 on <model>`.
5. Any other answer, success or error, goes back to the client as it came. The
   last account's answer is returned even when it is busy.

When no account can be used, the client gets `500 no usable account`. When
every account failed without an answer, it gets `502 all accounts failed`.

## Antigravity level variants

Antigravity lists one model several times, once per thinking level:
`gemini-3.8-flash-high`, `-medium`, `-low` and `-tiered`. intact lists such a
model once, as `gemini-3.8-flash`, and picks the variant from the effort the
request asks for:

| The request says | Variant used |
| --- | --- |
| nothing, or effort `none` / `auto` | the default: `-tiered` when listed, else the highest level |
| `reasoning_effort: "high"` (OpenAI), `reasoning.effort` (Responses), `output_config.effort` (Anthropic) | that level, or the nearest one the model has |
| `thinking: {type: "enabled", budget_tokens: n}` | ≤ 1500 → low, ≤ 6000 → medium, else high |
| `thinking: {type: "disabled"}` | the default |
| Gemini `thinkingConfig.thinkingLevel` / `thinkingBudget` | the level, or the budget as above |
| `minimal` / `xhigh`, `max` | `extra-low` (or the lowest) / `high` |

Other behaviour of variants:
- When every account refuses the chosen variant as busy, the request is tried
  once more on the default variant.
- The response header `X-Intact-Model` always names the model that answered.
- A full id such as `gemini-3.8-flash-high` still works, and follows the
  on/off switch of its base model.
- Models Google marks deprecated (listed, but refusing calls) are dropped from
  the list.

## Identity

Providers reached as a coding tool expect that tool's headers. intact sends the
identity recorded from the real tool's traffic, with its version constants in
`internal/provider/registry.go`:
- Codex CLI user agent and account header;
- the VS Code Copilot Chat headers;
- Antigravity IDE user agent;
- Claude Code user agent and beta flags.

Some providers answer differently by client version. Antigravity, for example,
lists the `-high/-medium/-low` variants only to IDE 2.11.0. Keep these
constants current.
