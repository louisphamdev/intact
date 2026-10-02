# Class A: the Antigravity protocol

This page records what the Antigravity CLI (`agy`) sends to Cloud Code Assist.
intact copies this shape when it translates a request for an Antigravity
account, and passes it through unchanged when the CLI itself is the client.

## How the capture was taken

The captures come from `agy` 1.2.7 on Windows, on 2026-10-02. Its User-Agent
names build `1.2.14`. The environment variable `CLOUD_CODE_URL` points the CLI
at a local recorder, and the recorder forwards each call to Google. The
recorder hides the token and keeps every other byte.

The token of the CLI belongs to the OAuth client
`1071006060591-tmhssin2h21lcre235vtolojh4g403ep`. intact signs in with the same
client, so a request from intact carries the identity of the CLI without a
mismatch.

## The calls

The CLI calls these paths on `https://daily-cloudcode-pa.googleapis.com`:

| Path | Body | Use |
| --- | --- | --- |
| `/v1internal:loadCodeAssist` | `{"metadata":{"ideType":"ANTIGRAVITY"}}` | The project of the account (`cloudaicompanionProject`). |
| `/v1internal:fetchAvailableModels` | `{"project":"…"}` | The models and the values of each model. |
| `/v1internal:streamGenerateContent?alt=sse` | The envelope below | One model call, streamed. |
| `/v1internal:retrieveUserQuotaSummary`, `listExperiments`, `fetchUserInfo`, `fetchAdminControls` | `{"project":"…"}` or empty | Quota, flags and the account. |

Every call carries these headers and no other client header:

- `User-Agent: antigravity/cli/1.2.14 (aidev_client; os_type=windows; arch=amd64; cl=990662481; auth_method=consumer)`
- `Authorization: Bearer <token>`
- `Content-Type: application/json`
- `Accept-Encoding: gzip`

## The envelope

The keys come in this order:

```json
{
  "project": "aicode-consumers",
  "requestId": "agent/<session uuid>/<unix ms>/<trajectory uuid>/<number of contents>",
  "request": {
    "contents": [{ "role": "user", "parts": [{ "text": "…" }] }],
    "systemInstruction": { "role": "user", "parts": [{ "text": "…" }] },
    "tools": [{ "functionDeclarations": [{ "name": "…", "description": "…", "parameters": { "type": "OBJECT" } }] }],
    "labels": {
      "last_step_index": "2",
      "model_enum": "MODEL_PLACEHOLDER_M318",
      "request_id": "<trajectory uuid>-1",
      "trajectory_id": "<trajectory uuid>",
      "used_claude": "false",
      "used_claude_conservative": "false",
      "used_non_gemini_model": "false"
    },
    "generationConfig": { "maxOutputTokens": 65536, "thinkingConfig": { "includeThoughts": true, "thinkingBudget": -1 } },
    "sessionId": "-3750763034362895579"
  },
  "model": "gemini-3.8-flash-high",
  "userAgent": "antigravity",
  "requestType": "agent"
}
```

The CLI fills these fields from the model list, not from fixed values:

| Field | Source in `fetchAvailableModels` |
| --- | --- |
| `generationConfig.maxOutputTokens` | `maxOutputTokens` of the model |
| `thinkingConfig.thinkingBudget` | `thinkingBudget` of the model: -1 for `-high`, 4000 for `-medium`, 1000 for `-low`, 1024 for Claude |
| `labels.model_enum` | `model` of the model |
| `labels.used_claude`, `used_claude_conservative` | `true` when `modelProvider` is `MODEL_PROVIDER_ANTHROPIC` |
| `labels.used_non_gemini_model` | `true` when `modelProvider` is not `MODEL_PROVIDER_GOOGLE` |

Other rules of the shape:

- The level is part of the model id (`gemini-3.8-flash-high`). The CLI sends no
  `thinkingLevel`.
- The CLI sends no `toolConfig`.
- A tool result is `{"functionResponse":{"id":…,"name":…,"response":{"output":"<text>"}}}`.
  For a Gemini model it is a turn of its own with the role `model`. For a
  Claude model it has the role `user`.
- Only the first `functionCall` of a turn carries a `thoughtSignature`. The CLI
  does not send thought text back for a Gemini model.
- Schema types are upper case: `OBJECT`, `STRING`, `INTEGER`.
- `trajectory_id` stays the same for every step of one conversation.
- A second request type, `checkpoint`, summarizes the session with
  `gemini-3.5-flash-lite`.

## Caching

A Claude model reports `cachedContentTokenCount`. In the capture, the second
call of a conversation read 13,576 of its 14,554 prompt tokens from the cache.
A Gemini model reports no cache count on this host.

## What intact does with the shape

- A translated request for an Antigravity account uses this shape. The values
  of each model come from the cached model list.
- When the CLI is the client, intact uses Bifrost: the bytes go as sent. Only
  the token and the project change. See [Bifrost](#bifrost-the-cli-to-an-antigravity-account).

## Bifrost: the CLI to an Antigravity account

The `antigravity` provider declares the User-Agent prefix of its client,
`antigravity/cli/`. When the `User-Agent` of a caller starts with this prefix,
intact does these steps:

- It accepts the CLI paths `/v1/v1internal:streamGenerateContent` and
  `/v1/v1internal:generateContent`, and sends each to the same path at Google.
- It keeps the headers and the body of the caller.
- It writes the project of the chosen account into the top-level `project`.
- It replaces `Authorization` with the token of the account.
- It keeps one conversation on one account, by `labels.trajectory_id`.
- It counts the tokens from `usageMetadata` of the stream.

The model entry gives the prefix as `bifrost_ua`, also for a level id such as
`antigravity/gemini-3.8-flash-high`. A Code Assist request cannot reach an
account of another provider. intact refuses it with 400.
