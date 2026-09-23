# Phase 1 verification

Date: 2026-09-20
Host: a 2 vCPU Debian server
Binary: cross-compiled from Windows, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, 10.8 MB, stripped, statically linked
Listen: `127.0.0.1:20140`
Provider under test: `groq`. The credential stayed on the test host.

## What was proven

### 1. The response is not altered

The same request was sent twice: once straight to `api.groq.com`, once through
`intact`. Both bodies were compared by their full key paths.

```
direct  keys: 28
proxied keys: 28
fields LOST by the proxy : NONE
fields ADDED by the proxy: NONE
direct  content: "OK"
proxied content: "OK"
```

Fields that survive include groq's own `queue_time`, `prompt_time`,
`completion_time`, `total_time` and `completion_tokens_details.reasoning_tokens` —
exactly the kind of field a translation layer drops.

### 2. A real model answers

```
POST /p/<id>/chat/completions   http=200
content: "OK."
usage: {"prompt_tokens":78,"completion_tokens":56,"total_tokens":134,
        "completion_tokens_details":{"reasoning_tokens":46}, ...}
```

### 3. Streaming arrives in chunks

```
curl -N ... stream=true
data: {"id":"chatcmpl-...","object":"chat.completion.chunk", ... ,"x_groq":{...
```

The provider-specific `x_groq` object is present, and the first chunk arrives
before the upstream finishes. A plain `io.Copy` in the handler makes this test
deadlock; the handler flushes each chunk instead.

### 4. Non-POST methods pass through

```
GET /p/<id>/models   http=200   models returned: 13
```

### 5. The credential never appears in a response

```
GET /accounts
{"accounts":[{"id":"...","provider":"groq","label":"Groq1","isActive":true}]}
```

### 6. Routing is exact

```
GET /        http=200   <title>intact</title>
GET /nope    http=404
```

## Memory

```
intact           17.6 MB RSS
```

The two processes of the gateway it replaces used 54.9 MB and 188.4 MB on the
same host.

## Two defects found only on the host

1. **The first port was already taken** by another local service, so the start
   failed with `bind: address already in use`. The test request then reached that
   other service, which answered with an HTML page. The listener moved to a free
   port.
2. **The model id in the client profile does not exist at groq.**
   `llama-3.3-70b-versatile` returns `model_not_found`; groq exposes
   `openai/gpt-oss-120b`. The first name is an alias of another gateway, not a
   provider id. `intact` returned the provider's 404 body unchanged, which is how
   the cause was identified in one request.

## Repeated on a reproducible binary

The first run above used a binary built from a dirty working tree. `go version -m`
reported `3a6e510e9923+dirty`, so the code that ran was not the code in any commit.

The binary was built again from a clean `15aa927` with `-trimpath`, which makes the
build reproducible: two builds of the same commit gave the same SHA-256, and the
file on the test host has that same digest. `go version -m` now reports
`vcs.revision=15aa927070c9` and `vcs.modified=false`.

The live check was repeated on that binary:

```
POST /p/<id>/chat/completions   http=200
top-level keys: choices, created, id, model, object, service_tier,
                system_fingerprint, usage, usage_breakdown, x_groq
message keys  : content, reasoning, role
usage         : queue_time, prompt_tokens, prompt_time, completion_tokens,
                completion_time, total_tokens, total_time,
                completion_tokens_details.reasoning_tokens
RSS           : 13.7 MB
```

`message.reasoning` is the field that the gateway it replaces removes. It
arrives here.

CAUTION: Build a release with `-trimpath`. Without it the build embeds the path of
the source directory, so the same commit gives a different digest on each machine
and you cannot prove which code runs.

## Not yet covered

- Class A providers (antigravity, codex, github, claude). They need captured
  traffic from the genuine tool first.
- Quota endpoints. Phase 3.
- Adding a connection through the dashboard. The credential was inserted directly
  into the database for this test.
