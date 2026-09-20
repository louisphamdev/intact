# Class A: the Codex protocol

A Codex completion is **not** an HTTP POST. The CLI opens a WebSocket. Any gateway
that only serves `POST /responses` is answering a call that Codex does not make.

Everything below was read from a capture of the genuine tool.

## How the capture was taken

Tool: Codex CLI, `codex exec`, host `chatgpt.com`, ChatGPT authentication.
Date: 2026-09-20.
Method: the `blindfold` proxy of `llm-switcher`, capture mode, which reads the
WebSocket frames from a copy of the stream.

```bash
node blindfold/blindfold.mjs --host chatgpt.com --prefix /no-gateway \
  --port 3459 --certs blindfold/certs --capture /tmp/codex-captures
```

The tool was then started with `HTTPS_PROXY` and `CODEX_CA_CERTIFICATE` set for
that process only.

## The transport

```
GET /backend-api/codex/responses    HTTP/1.1 -> 101 Switching Protocols
```

The frames use `permessage-deflate` with context takeover. A reader must keep the
inflate window between messages. Inflating each message on its own decodes the
first and fails on every later one.

## Messages

Client sends `response.create`, which carries the model and the input:

```json
{"type":"response.create","model":"gpt-5.6-sol","input":[...]}
```

Server answers with this sequence:

```
codex.rate_limits
codex.response.metadata
response.created
response.in_progress
responsesapi.websocket_timing
response.completed
```

## Quota arrives without being asked for

`codex.rate_limits` is the first message of every turn. Nothing has to poll a
quota endpoint: the numbers come with the answer.

```json
{"type":"codex.rate_limits",
 "plan_type":"k12",
 "rate_limits":{
   "allowed":true,
   "limit_reached":false,
   "primary":  {"used_percent":2, "window_minutes":300,  "reset_after_seconds":12066, "reset_at":1789911012},
   "secondary":{"used_percent":32,"window_minutes":10080,"reset_after_seconds":495876,"reset_at":1790394822}},
 "code_review_rate_limits":null,
 "additional_rate_limits":null,
 "credits":{"has_credits":false,"unlimited":false,"balance":"0"},
 "promo":null}
```

`primary` is the 5-hour window and `secondary` is the 7-day window. Both give a
percentage and an absolute reset time.

This is what a quota display needs, and it costs no extra request. A gateway that
converts the stream to another protocol drops this message, because no other
protocol has a place for it.

## What this means for intact

- A `codex` provider must proxy a WebSocket, not only HTTP. The current proxy
  handles HTTP alone.
- Quota needs no endpoint of its own. Read `codex.rate_limits` as it passes and
  store the last value per connection.

Neither is implemented yet.
