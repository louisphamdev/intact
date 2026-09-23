# Security

## Three callers

| Who | Gate | Reaches |
| --- | --- | --- |
| A person | Sign-in with a TOTP code from an authenticator app (no password) → a signed session cookie | the dashboard and its routes |
| A machine that holds `INTACT_API_TOKEN` | The master token as `Authorization: Bearer <token>` or `x-api-key: <token>` | `/v1`, `/api/*`, `/mcp` |
| A machine that holds a dashboard key | The key as `Authorization: Bearer <key>` or `x-api-key: <key>` | `/v1`, and the reads of `/api` and `/mcp` that are not admin-only (below) |

A dashboard key calls models. It does not manage intact. The session and the
master token are admin. A key is not, and no key name makes it one.

The API key is never forwarded to a provider. intact removes `Authorization`
and `x-api-key` from the incoming request and sets the account's own credential.

Without `INTACT_TOTP_SECRET`, intact does not start. A loopback bind is not a
gate: the reference deployment sends tunnel traffic to that same loopback port.
To start an ungated server, pass `-insecure-no-auth` or set
`INTACT_INSECURE_NO_AUTH=1`. With no gate, intact answers 403 to every request
whose `Host` header is not `127.0.0.1`, `localhost` or `[::1]`, which stops DNS
rebinding.

## Sign-in limits

The dashboard takes a TOTP code and nothing else. intact charges every attempt
to one budget before it tests the code. A correct code gives its charge back.

| Attempt | Budget |
| --- | --- |
| The owner path, only with `INTACT_OWNER_LOOPBACK=1`: a loopback connection with no `CF-Connecting-IP` and no `X-Forwarded-For` header | none |
| A browser with the `intact_device` cookie of an earlier sign-in | 10 wrong codes per browser in 24 hours |
| Every other attempt | 5 wrong codes per address in 15 minutes, and 100 wrong codes in 24 hours |

The public budget admits 3000 codes in 30 days. One guess matches with 3 of
1,000,000, because intact accepts three time windows. A month of attacks from
new addresses gives a chance of 0.9 percent.

The owner path is off by default. Without it, a loopback attempt with no
forwarding header uses the public budget.

When `INTACT_OWNER_LOOPBACK=1` is set, the owner path has no budget. A stranger
who fills the public budget cannot keep the owner out. The owner opens an SSH
tunnel to the loopback port (`ssh -L`), and signs in. This decision reads the
connection and the presence of the two headers. It never reads a header value,
and it never reads the code. Cloudflare always writes `CF-Connecting-IP`, so a
request through the tunnel cannot take this lane.

CAUTION: Set `INTACT_OWNER_LOOPBACK=1` only when every proxy in front of intact
writes `CF-Connecting-IP` or `X-Forwarded-For`. A proxy that writes neither
header, for example nginx with a plain `proxy_pass`, sends every internet caller
from loopback. With the flag set, each of those callers can try TOTP codes with
no limit.

The `intact_device` cookie grants no access. intact sets it after each sign-in,
with `HttpOnly` and `SameSite=Strict`. The cookie carries a random id and an
HMAC over that id with the session key. A stranger cannot sign the cookie, so a
stranger cannot spend the budget of a browser.

A code is single use. intact records the time step of the last accepted code,
and refuses a code from that step or an older one. A code read over a shoulder
cannot open a second session.

## A session lives to its end

A sign-in gives a signed cookie with a lifetime of `INTACT_SESSION_TTL` seconds
(12 hours by default). intact keeps no list of open sessions, so it cannot
revoke one. **Logout** deletes the cookie in the browser. It does not stop a
copy of that cookie.

If a session cookie is stolen, set a new `INTACT_SESSION_KEY` and restart
intact. Every cookie signed with the old key then fails, and every browser signs
in again. When `INTACT_SESSION_KEY` is empty, intact makes a random key at each
start, so a restart alone ends every session.

## Cross-site and framing defense

In both modes, intact answers 403 to a POST, PUT or DELETE that carries an
`Origin` header of another host, an `Origin` of `null`, or the header
`Sec-Fetch-Site: cross-site`. The comparison reads the host only, so a
TLS-terminating tunnel keeps working. A request with no `Origin` header passes,
because an API client sends none.

Every response carries `Content-Security-Policy: frame-ancestors 'none'`,
`X-Frame-Options: DENY` and `X-Content-Type-Options: nosniff`.

## API keys

Keys are created under **Endpoint → API keys**:
- A key looks like `sk-intact-…`.
- It is shown in full once. After that, **Reveal** shows it again.
- It can be switched off or deleted at any time.
- A key has a `trusted` flag (default false). Only the session can change this flag.

`INTACT_API_TOKEN` in the environment is the master token. It is not shown in
the dashboard, and it reaches every route.

## Contract Lab storage and trust

intact never stores raw prompt text or answer text.
Every payload is reduced to structural shape records.
Strings keep only their byte length and a 12-byte HMAC-SHA256 hash.
The server generates the HMAC secret on first start and keeps it in the database.
Known enum values on an allowlist keep their string value.
Object keys that contain special characters become `{*}`.

Public routes and untrusted callers cannot read hashes or string lengths.
Only trusted traces can write shared learned contracts, trigger judge evaluations, or open findings.
A trusted caller is the session, the master token, or an API key marked as trusted.
When an operator disables the trusted flag on a key, all open traces for that key move to `revoked`.

## What a dashboard key cannot do

These routes answer 403 to a dashboard key. Only the session cookie or the
master token passes:

- `POST /api/accounts/{id}/active`, `POST /api/accounts/{id}/test`
- `POST /api/providers/{id}/rotation`, `POST /api/providers/{id}/model-policy`
- `POST /api/providers/{id}/models/active`, `/models/delete`, `/models/test`
- `POST /api/rankings/refresh`, `POST /api/rankings/alias`
- `POST /api/provider-defs`, `DELETE /api/provider-defs/{id}`
- `POST /api/filters`, `DELETE /api/filters/{id}`, `POST /api/filters/{id}/delete`
- `POST /api/drift/ack`, `POST /api/drift/seed`, `POST /api/drift/review`
- every `/api/notify` route, and `POST /api/errors/review`

Four reads answer a dashboard key differently:

- `GET /api/provider-defs` and `GET /api/provider-defs/{id}` replace the OAuth
  client secret and every static header value with `••••`. The stored value
  does not change, so an admin can edit a declared provider and save it again.
- `GET /api/notify` answers 403. For Slack, Discord and ntfy the webhook url is
  the credential, so the channel list is admin-only.
- `GET /api/errors/{id}` and `GET /api/errors` show the stored request and
  answer bodies only for the errors that this key caused. intact records the
  key id with each error. An error stored before this change has no key id, so
  only the session and the master token read its bodies.
- `GET /api/drift/changes` clears the recorded sample text. Only the session
  and the master token read sample text.

Over `/mcp` the same rule applies. A dashboard key gets an error from
`set_account_active`, `set_models_active`, `test_account`, `set_model_policy`,
`put_provider_def`, `delete_provider_def`, `add_filter`, `update_filter`,
`delete_filter`, `ack_drift_changes`, `drift_review`, `error_review`,
`put_notify_channel`, `test_notify_channel`, `delete_notify_channel`,
`list_notify_channels` and `get_error`. The tools `list_provider_defs` and
`list_drift_changes` mask the same data as the routes.

## Credentials

- **Where they live.** Account credentials are in the SQLite database: API keys,
  OAuth access and refresh tokens, Copilot's GitHub token. Keep the file and its
  backups private (mode 600).
- **They are never shown.** A credential never appears in an account listing,
  the API or MCP, and never in a log.
- **OAuth tokens refresh in place.** When a provider rotates the refresh token,
  the new one is stored.
- **Shared accounts.** An OAuth account shared with another gateway can be
  broken by a rotation: when one side refreshes, the other side's refresh token
  may stop working. Codex rotates. Google (Antigravity) does not.

## What intact changes in a request

intact changes only what it must:
- It sets the account's credential and the provider's identity headers.
- It removes the prefix from the model name.
- It translates when the client's and the provider's shapes differ.
- It applies the [blacklist](blacklist.md).

It removes `Host`, `Accept-Encoding` and hop-by-hop headers. Everything else in
the request, and everything in the answer, is left as it is.
