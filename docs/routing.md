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
- `GET /v1/models` lists every model that is on, as `<provider>/<model>`. When
  the provider's list gives token limits, an entry also has `context_length`,
  `max_input_tokens` and `max_output_tokens`, plus `compact_window` for a model
  that publishes a window to compress at
  ([Models](models.md#token-limits)).

## Request shapes and translation

| Path | Client shape |
| --- | --- |
| `/v1/chat/completions` | OpenAI Chat Completions |
| `/v1/messages` | Anthropic Messages |
| `/v1/messages/count_tokens` | Anthropic token count. It is estimated locally when no Anthropic account serves the model. |
| `/v1/responses` | OpenAI Responses (Codex CLI) |
| `/v1/v1internal:streamGenerateContent`, `/v1/v1internal:generateContent` | Code Assist (Antigravity CLI). It reaches an Antigravity account only. |
| any other path (`/v1/systemone`, …) | Passed through to the provider at the same path |

Each provider speaks one shape: OpenAI, Anthropic, Responses (Codex), or Gemini
inside Antigravity's envelope. When the client's shape differs from the
provider's, intact translates:
- the request, through OpenAI Chat as the common form;
- the answer, whole or as a server-sent-event stream, back into the client's
  shape.

When the two shapes are the same, the bytes pass through untouched, at the
provider's own path, with two exceptions:
- The answer names the model id that the client sent, prefix included. intact
  removes the prefix on the way in, so it puts the prefix back on the way out.
- An OpenAI Chat stream always asks the provider for its usage, so that intact
  can count the tokens. If the client did not ask for usage, intact removes the
  usage chunk and the `"usage":null` fields before the client reads them.

When the provider reasons and the client enabled thinking on `/v1/messages`,
the reasoning arrives as `thinking` blocks with an empty signature.

Details worth knowing:
- **Codex and Antigravity answer only as a stream.** When the client did not
  ask to stream, intact collects the stream and returns one whole answer.
- **Copilot** serves Claude models on `/v1/messages`. It serves some models
  only on `/responses`: intact learns those from Copilot's refusal and sends
  them there.
- **Antigravity** requests are wrapped in its envelope, in the shape of the
  Antigravity CLI: project, labels, session id, and a request id per call. The
  output limit and the thinking budget come from the model list. Gemini thought
  signatures are kept between turns, so tool calls survive a round trip.
  [Antigravity](class-a-antigravity.md)
- **Tool calls, images and reasoning** are carried across shapes where both
  sides support them.
- **Responses tools** reach a chat provider as function tools. A tool in a
  `namespace` gets the name `<namespace>__<name>`. A `custom` (freeform) tool
  gets one string argument, `input`. intact gives each call back to the client
  as the kind of tool the client declared. Hosted tools, such as `web_search`,
  run only at OpenAI, so intact does not send them. Encrypted reasoning items
  are also not sent, because no other provider can read them.
- **Structured output** is carried too. A JSON schema for the answer goes as
  `response_format` (OpenAI), `text.format` (Responses), `output_config.format`
  (Anthropic) or `responseMimeType` with `responseSchema` (Gemini). Anthropic
  has no JSON mode without a schema, so such a request reaches it as plain text.

## Rotation and failover

For each call intact builds the list of accounts that serve the model, and
picks the account to try first by the provider's **rotation**. It is set at
the top of the provider's Connections card, or at
`/api/providers/{id}/rotation`:

| Setting | Behaviour |
| --- | --- |
| Round robin, 1 request a turn (default) | Accounts take turns on every request. |
| Round robin, N requests a turn | Each account serves N requests in a row, then the next takes over. This keeps a provider's prompt cache warm on one account. |
| Fallback (round robin off) | The first account serves every request. The next one is tried only when it fails or is busy. |

- **Priority.** The ▲▼ buttons set the accounts' order, which both modes
  follow.
- **Standby.** An active account marked **Standby** takes no turns. intact
  tries it only after every other active account failed or was busy, in both
  modes. An account that is off is never tried. Turning an account off clears
  its standby mark.
- **Next account.** The card marks the account that takes the next request,
  with its place in the turn (`next · 2/3`).
- **Pools across providers.** A bare model listed by several providers pools
  their accounts. Such a pool turns on every request, per model.
- **Conversations.** Every provider in the pool keeps a prompt cache per
  account, so a conversation that moves to another account writes its whole
  prefix again. intact sends each conversation back to the account that last
  answered it. The other accounts follow in rotation order, and standby
  accounts come last. A new conversation takes the rotation, so the load
  spreads by conversation, not by request. intact names a conversation by:
  - the API key of the caller, so two keys never share one;
  - the session that the client names, when it names one: the
    `X-Claude-Code-Session-Id` header, the `session_id` in `metadata.user_id`,
    the OpenAI `prompt_cache_key` field, or the `labels.trajectory_id` of the
    Antigravity CLI. A plain `user_id` adds nothing;
  - the first message that is not a system message (the next bullet). In a
    Code Assist body, this is the first entry of `request.contents`.

  intact keeps a conversation for 1 hour after its last request, in memory. A
  restart forgets the conversations. A body with no messages, no input and no
  contents (an embedding, a model list) takes the rotation.
- **A compaction starts a new conversation.** The provider caches the start
  of the conversation, so the first message changes only when the cache
  cannot hit. intact does not need to know whether a new first message comes
  from a compaction or from a new session, because both take the rotation:
  - A compaction request (`/compact`, auto-compact, or one that Claude Code
    prepares in the background) still starts with the old first message. It
    goes to the account that holds the cache.
  - After the compaction, the conversation starts with the summary. No
    account holds a cache for it, so it takes the rotation and then stays on
    the account that answers.
  - A side request of the client in the same session has a first message of
    its own. It does not move the conversation.
  - A client that keeps its first messages when it shortens the history (for
    example Hermes) stays on its account, where the start is still cached.
  - A client that changes its first message on every turn takes the rotation
    on every turn.
  - `cache_control` marks are not part of the name, because the client moves
    them every turn.
- **Spent quota.** An account whose [quota](quota-and-usage.md) for the model
  is spent is skipped until the window of that quota resets. Then the account
  takes turns again by itself. The check reads the quota cache only. When a
  `429` arrives and the cached quota was read before it and still shows quota
  left, intact reads the quota again, once per account in 30 seconds. Thus the
  first real limit skips the account from the next request on. A fake `429`
  leaves the quota as it is, so it skips nothing. When every account is spent,
  intact skips none of them, and the client gets the provider's answer.

Then, for each account, starting there:

1. An OAuth token close to expiry is refreshed before use.
2. The request is sent, after the [blacklist](blacklist.md) has run on the exact
   bytes that will leave. A request from the provider's own client (Bifrost)
   gets no blacklist.
3. A `401` from an OAuth account forces a token refresh and one retry.
   Copilot's exchanged token is re-exchanged instead.
4. A busy answer (`429`, `500`, `503`, `409`) moves to the next account and
   writes a log line: `connection <id>: <provider> answered 429 on <model>`.
   A `404` also moves to the next account, because model access differs per
   account: one Claude account can refuse a model that another one serves.
5. Any other answer, success or error, goes back to the client as it came. The
   last account's answer is returned even when it is busy.

Every error answer, and every attempt that got no answer, is kept in the
[error log](errors.md), including the attempts intact moved on from.

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
- Antigravity CLI user agent;
- Claude Code user agent and beta flags.

A request from the provider's own client keeps its own identity: intact
changes only the credential ([Bifrost](class-a-claude.md#bifrost-claude-code-to-a-claude-code-account)).

The headers that a proxy in front of intact adds never reach a provider:
`Cf-*`, `Cdn-Loop`, `X-Forwarded-*`, `X-Real-Ip`, `Forwarded`,
`True-Client-Ip`, and `Cookie`. They name the person behind the request.

A request that intact translates to a provider of another shape does not
carry the client's query string. The query belongs to the client's API.

Some providers answer differently by client version. Keep these constants
current.
