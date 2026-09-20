# Phase 1 verification

Date: 2026-09-20
Host: VPS `copilot`, 2 vCPU Xeon E-2236, Debian
Binary: cross-compiled from Windows, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, 10.8 MB, stripped, statically linked
Listen: `127.0.0.1:20140`
Provider under test: `groq`, credential taken from the existing 9router database **on the VPS**; it never left that host.

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
9router core     54.9 MB RSS
9router next-server 188.4 MB RSS
```

## Two defects found only on the VPS

1. **Port 20130 was already taken** by a local python service, so the first start
   failed with `bind: address already in use` and the test request reached that
   other service, which proxied it to 9router and returned a 9Router HTML page.
   Moved to 20140.
2. **The model id in the llm-switcher profile does not exist at groq.**
   `llama-3.3-70b-versatile` returns `model_not_found`; groq exposes
   `openai/gpt-oss-120b`. That name is a 9router alias, not a provider id — the
   same class of defect the `patch-antigravity-tiered-ids.py` patch corrects.
   `intact` returned the provider's 404 body verbatim, which is how the cause was
   identified in one request.

## Not yet covered

- Class A providers (antigravity, codex, github, claude). They need captured
  traffic from the genuine tool first.
- Quota endpoints. Phase 3.
- Adding a connection through the dashboard. The credential was inserted directly
  into the database for this test.
