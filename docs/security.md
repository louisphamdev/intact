# Security

## Two gates

| Who | Gate | Reaches |
| --- | --- | --- |
| A person | Sign-in with a TOTP code from an authenticator app (no password) → a signed session cookie | the dashboard and its routes |
| A machine | An API key as `Authorization: Bearer <key>` or `x-api-key: <key>` | `/v1`, `/api/*`, `/mcp` |

The API key is never forwarded to a provider. intact removes `Authorization`
and `x-api-key` from the incoming request and sets the account's own credential.

Without `INTACT_TOTP_SECRET`, intact has no gate and refuses to listen anywhere
but loopback.

## API keys

Keys are created under **Endpoint → API keys**:
- A key looks like `sk-intact-…`.
- It is shown in full once. After that, **Reveal** shows it again.
- It can be switched off or deleted at any time.

`INTACT_API_TOKEN` in the environment also works as a key, and is not shown in
the dashboard.

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
