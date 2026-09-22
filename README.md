# intact

A small credential proxy for LLM providers. It holds each account's credential on
one host, forwards a request to the provider unchanged, and records the token
usage. It is a light alternative to a large gateway: it keeps the basic features
and drops the parts that alter the payload.

## What it does

- **Passthrough proxy.** intact adds the account's identity and credential to a
  request, then sends the provider's bytes back to the caller unchanged. The
  caller speaks the provider's own protocol.
- **One account, a provider, or a group.** Call `/p/<id>/…` to use one account,
  `/r/<provider>/…` to let intact pick an active account of that provider, or
  `/g/<group>/…` to pick from a named group of accounts that can span providers
  (see [Groups](#groups)). `/r` and `/g` fail over on a busy status
  (429/500/503/409).
- **Usage counter.** intact reads the `usage` object from each response and
  keeps a daily total per account and model. It reads the response; it never
  changes it.
- **Model list.** The dashboard shows the models of a provider, read server-side.
- **Authentication.** A person signs in with a TOTP code (an authenticator app).
  A machine sends a bearer token. There is no password.
- **OAuth refresh.** For an OAuth account, intact refreshes an expired or revoked
  access token with the stored refresh token (see [OAuth](#oauth)).

## Status

| Area | State |
| --- | --- |
| Core proxy, round-robin, failover | done, tested, live |
| Groups across providers (round-robin / fallback, model override) | done, tested |
| Usage counter (JSON and SSE, gzip, cache tokens) | done, tested, live |
| TOTP login + bearer token gate | done, tested, live |
| Connection CRUD + model list (dashboard) | done, tested, live |
| Providers: groq, claude, nvidia, openrouter, typesafe | registered |
| OAuth refresh mechanism (expiry, rotation, 401 self-heal) | done, tested |
| OAuth: antigravity refresh | wired and verified safe (Google does not rotate) |
| OAuth: claude, codex refresh | wired; live activation needs a decision (see below) |
| OAuth: github token exchange | not built |
| Proxy endpoints for antigravity, codex, github | not built (need captured traffic) |
| Interactive OAuth login (add a new account) | not built |

The remaining OAuth work and the reasons it is blocked are in
`temp/intact-oauth-spec.md`.

## Build and run

The store uses `modernc.org/sqlite`, a pure-Go driver, so `CGO_ENABLED=0` builds
a static binary.

```bash
# build for this host
CGO_ENABLED=0 go build -o intact ./cmd/intact

# cross-build for a Linux server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o intact-linux ./cmd/intact

# run
./intact -db ./intact.db -addr 127.0.0.1:20142
```

Flags:

- `-addr` — the listen address (default `127.0.0.1:20130`).
- `-db` — the SQLite file (default `intact.db`).
- `-enroll` — print a new TOTP secret and its `otpauth://` URI, then exit.

The server refuses a non-loopback address when no TOTP secret is set, so it never
exposes a credential without a gate.

## Configuration

intact reads these environment variables. Set them in a systemd `EnvironmentFile`
or the shell.

| Variable | Meaning |
| --- | --- |
| `INTACT_TOTP_SECRET` | The base32 TOTP secret for the login. When empty, the server has no gate (loopback only). |
| `INTACT_API_TOKEN` | The bearer token a machine sends to `/p` and `/r`. |
| `INTACT_SESSION_KEY` | The key that signs the session cookie. A random key is generated when empty, so a restart then ends every session. |
| `INTACT_SESSION_TTL` | The session lifetime in seconds (default 43200). |

To enroll:

1. Run `./intact -enroll`.
2. Add the printed `otpauth://` URI to an authenticator app.
3. Put the printed `INTACT_TOTP_SECRET` in the environment file.

## Endpoints

| Method and path | Gate | Purpose |
| --- | --- | --- |
| `POST/GET /p/{id}/{path...}` | bearer token | Proxy to one account. |
| `POST/GET /r/{provider}/{path...}` | bearer token | Round-robin across a provider's active accounts, with failover. |
| `POST/GET /g/{group}/{path...}` | bearer token | Round-robin or fallback across a group's active members, with failover. |
| `GET /` | session | The dashboard: Endpoint, Providers (accounts and models per provider), Combos, Usage. |
| `GET /accounts` | session | The connection list as JSON. |
| `POST /accounts` | session | Add a connection (`provider`, `label`, `secret`). |
| `POST /accounts/{id}/delete` | session | Delete a connection. |
| `GET /accounts/{id}/models` | session | The provider's model list for one connection. |
| `POST /accounts/{id}/active` | session | Turn a connection on or off (`{"active":bool}`). |
| `GET /groups` | session | The groups and their members as JSON. |
| `POST /groups` | session | Create or replace a group (JSON, see below). |
| `POST /groups/{name}/delete` | session | Delete a group; its connections stay. |
| `GET /providers` | session | The provider ids this build can proxy. |
| `GET /usage` | session | The daily usage totals as JSON. |
| `GET /login`, `POST /login`, `POST /logout` | open | The TOTP login. |

## Groups

A group (a "combo" in the dashboard) pools models behind one name. Its members
can belong to different providers, so a combo can spread load over groq, nvidia
and openrouter at once.

```json
POST /groups
{"name": "free-llama", "strategy": "round-robin", "members": [
  {"provider": "groq", "model": "llama-3.3-70b-versatile"},
  {"provider": "nvidia", "model": "meta/llama-3.3-70b-instruct"},
  {"connectionId": "…one openrouter account…", "model": "meta-llama/llama-3.3-70b-instruct:free"}
]}
```

- **Members.** A member with a `provider` and no `connectionId` uses every
  active account of that provider, rotated like `/r`. A member with a
  `connectionId` is pinned to that account.

- **Strategy.** `round-robin` rotates the first member tried on each call.
  `fallback` always tries the members in order, so the first takes the load and
  the rest serve only when it is busy.
- **Model override.** The same model has a different id at each provider. A
  member with a `model` gets the top-level `model` of the request body replaced
  for that member only. The value is spliced into the original bytes, so every
  other byte of the body is sent as the caller wrote it. A member with no model
  gets the body unchanged.
- **Skipped members.** An inactive connection, or a provider this build does
  not register, is skipped. Deleting a connection removes its pinned members.
- **Protocols.** intact does not translate between APIs. Put only members that
  accept the same request shape in one group (all OpenAI-compatible, or all
  Anthropic). The dashboard warns when a group mixes them.

## Providers

A provider entry holds the base URL and the auth header. `internal/provider`
registers groq, claude, nvidia, openrouter, and typesafe. TypeSafe is not
OpenAI-shaped, but it uses a bearer token and returns a `usage` object, so the
passthrough carries it without a translation layer.

A connection stores its credential and, for OAuth, its refresh configuration. The
credential never leaves the host and never appears in the connection listing.

## OAuth

The imported accounts of a class-A provider (claude, codex, antigravity, github)
authenticate with an OAuth token that expires. intact keeps them working with the
refresh grant:

- `internal/oauth.Refresh` runs the `refresh_token` grant. It returns a rotated
  refresh token when the provider sends one, so intact stays valid across a
  rotation.
- `secretFor` refreshes a token at or near its recorded expiry before use.
- On a provider 401 for an OAuth account, intact force-refreshes and retries once,
  which recovers a token the provider revoked early.

**antigravity** is wired and verified: a live test showed Google returns a fresh
access token and does not rotate the refresh token, so intact refreshing an
antigravity account does not affect another gateway that shares it.

**claude and codex** are wired, but a live refresh may rotate the refresh token at
Anthropic or OpenAI. A rotation would invalidate the same token in another gateway
that holds it. So the live activation is a decision for the owner, not a default.

## Usage tracking

intact records only a daily row per account and model. A large response passes
through, and intact reads its head and tail to find the `usage` object, which
bounds the memory it holds. It forces `Accept-Encoding: identity` to the upstream
so the response is never compressed in a form the reader cannot parse.

## Deployment

The reference deployment runs intact as a systemd service bound to loopback, and
exposes it through a Cloudflare tunnel with the built-in TOTP login in front. See
`temp/intact-deploy.sh` for the service unit and the tunnel ingress steps.

## Tests

```bash
go test ./...
go vet ./...
```

Every package passes. The tests cover the usage parser, the store, the proxy tap,
the round-robin failover, the auth gate, and the OAuth refresh path.
