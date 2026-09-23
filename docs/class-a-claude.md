# Class A: the `claude` provider

A class A provider impersonates a real tool. The upstream answers a recognised
tool differently from an unknown client, so the headers are part of the contract.

Everything below was read from a capture of the genuine tool. It was not taken
from documentation and it was not guessed.

## How the capture was taken

Tool: Claude Code 2.1.278, the `sdk-cli` entry point.
Date: 2026-09-20.
Method: the `blindfold` proxy of the `llm-switcher` repository, in capture mode.

```bash
bash blindfold/make-certs.sh api.anthropic.com /tmp/anthropic-certs
node blindfold/blindfold.mjs --host api.anthropic.com --prefix /no-gateway \
  --port 3458 --certs /tmp/anthropic-certs --capture /tmp/anthropic-captures
```

Then the tool was started with `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS` set for
that process only.

CAUTION: A capture holds the prompts and the source code that the tool sent. Keep
the directory private. The proxy replaces credential headers with `<redacted>`,
but it does not redact a body.

## Request headers

| Header | Value | Class |
| --- | --- | --- |
| `Authorization` | the OAuth token | credential |
| `User-Agent` | `claude-cli/2.1.278 (external, sdk-cli)` | identity |
| `X-App` | `cli` | identity |
| `Anthropic-Dangerous-Direct-Browser-Access` | `true` | identity |
| `Anthropic-Version` | `2023-06-01` | default |
| `Anthropic-Beta` | 16 flags, see `registry.go` | default |

Identity always replaces what the caller sent. A default only fills a header that
the caller left out, because a beta flag selects a feature and the caller must
keep control of it.

The first two flags are `claude-code-20250219` and `oauth-2025-04-20`. The
upstream refuses an OAuth token without them, so they are part of the credential.

The tool also sends `x-claude-code-session-id`, `x-client-request-id` and ten
`x-stainless-*` headers. They are diagnostic. The proxy forwards whatever the
caller sends and adds none of them.

## Request body

```
model              "claude-opus-5"
messages           array
system             array of {type, text}
tools              array of {name, description, input_schema}
metadata           {user_id}
max_tokens         64000
thinking           {type, display}
context_management {edits}
output_config      {effort}
diagnostics        {previous_message_id}
stream             true
```

## Response

`content-type: text/event-stream`, and the body is `gzip` when the caller accepts
it. The event order is:

```
message_start -> content_block_start -> ping -> content_block_delta
              -> content_block_stop -> message_delta -> message_stop
```

`message_start` carries the full usage, and `message_delta` carries the final
usage:

```json
{"input_tokens":2,
 "cache_creation_input_tokens":0,
 "cache_read_input_tokens":64573,
 "cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},
 "output_tokens":5,
 "output_tokens_details":{"thinking_tokens":0},
 "service_tier":"standard",
 "inference_geo":"not_available"}
```

`intact` returns these bytes unchanged, so every field arrives at the caller.
`cache_creation`, `service_tier` and `inference_geo` have no equivalent in the
OpenAI schema; a gateway that converts the response must drop them.

## Token refresh

`intact` refreshes the OAuth token. When the access token expires, it sends the
refresh-token grant to Anthropic and stores the new pair. When Anthropic rotates
the refresh token, the new one is stored too. See [Providers](providers.md).

To add a connection, insert the token as the secret of a `claude` connection. The
token never appears in a response: `/accounts` returns the id, the provider, the
label and the active flag, and nothing else.
