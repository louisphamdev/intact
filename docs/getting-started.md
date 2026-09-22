# Getting started

## Build

intact is one static Go binary. The dashboard is embedded in it, and the store is
SQLite through a pure-Go driver, so no C toolchain is needed.

```bash
CGO_ENABLED=0 go build -o intact ./cmd/intact

# for a Linux server from another OS
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o intact-linux ./cmd/intact
```

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `127.0.0.1:20130` | Listen address. |
| `-db` | `intact.db` | SQLite file. It is created with its tables on first start and migrated on later starts. |
| `-enroll` | | Print a new TOTP secret and its `otpauth://` URI, then exit. |

intact refuses to listen on a non-loopback address without a TOTP secret, so it
never exposes credentials without a gate.

## Environment

| Variable | Meaning |
| --- | --- |
| `INTACT_TOTP_SECRET` | Base32 TOTP secret for the dashboard sign-in. Empty means no gate, and then only loopback is allowed. |
| `INTACT_API_TOKEN` | A token machines may send to `/v1`, `/api` and `/mcp`. Keys created in the dashboard work the same way. Optional. |
| `INTACT_SESSION_KEY` | Key that signs the session cookie. When empty, a random key is made at start, so a restart ends every session. |
| `INTACT_SESSION_TTL` | Session lifetime in seconds (default 43200). |
| `INTACT_ANTIGRAVITY_CLIENT_SECRET` | The Google OAuth client secret the Antigravity sign-in needs. Needed only to add Antigravity accounts. |

### Enroll the sign-in

1. Run `./intact -enroll`.
2. Add the printed `otpauth://` URI to an authenticator app.
3. Put the printed `INTACT_TOTP_SECRET=…` line in the environment file.

## First account and first call

1. Open the dashboard and sign in with the six-digit code.
2. **Providers → Add**: pick a provider from the list.
   - For an API-key provider, paste the key.
   - For a coding-tool sign-in, follow the sign-in steps. See
     [Providers](providers.md#sign-in-flows).
3. **Endpoint → API keys → Create**: copy the `sk-intact-…` key. It is shown
   again only when you press Reveal.
4. Call it:

```bash
# OpenAI shape
curl https://intact.example/v1/chat/completions \
  -H "Authorization: Bearer $INTACT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"groq/llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}'

# Anthropic shape: any model, including non-Anthropic ones
curl https://intact.example/v1/messages \
  -H "x-api-key: $INTACT_KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d '{"model":"codex/gpt-5.5","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}'
```

Set a client's base URL as follows:
- OpenAI SDKs and tools: `https://intact.example/v1`
- Anthropic SDKs and Claude Code: `https://intact.example`

## Deployment

The reference deployment runs intact under systemd on loopback, and exposes it
through a Cloudflare Tunnel. The TOTP sign-in guards the dashboard, and API keys
guard everything else.

`/etc/systemd/system/intact.service`:

```ini
[Unit]
Description=intact usage proxy
After=network.target

[Service]
EnvironmentFile=/opt/intact/intact.env
ExecStart=/opt/intact/intact -db /opt/intact/intact.db -addr 127.0.0.1:20142
Restart=on-failure
WorkingDirectory=/opt/intact

[Install]
WantedBy=multi-user.target
```

`/opt/intact/intact.env` (mode 600) holds `INTACT_TOTP_SECRET`, and optionally
`INTACT_API_TOKEN` and `INTACT_SESSION_KEY`.

Cloudflare Tunnel ingress:

```yaml
ingress:
  - hostname: intact.example
    service: http://127.0.0.1:20142
```

### Upgrading

1. Back up the database with SQLite's backup API, not a file copy. A copy can
   miss what is still in the write-ahead log.

   ```bash
   sqlite3 /opt/intact/intact.db ".backup /opt/intact/backup/intact-$(date +%F-%H%M).db"
   ```

2. Keep the old binary for a rollback (`intact.prev`), install the new one, and
   restart: `systemctl restart intact`.

Schema changes are applied at start and are additive.

## Background work

After start, intact runs these jobs on its own:
- It fetches each provider's model list every hour and deletes models the
  provider dropped. See [Models](models.md#fetching-the-list).
- It re-tests providers that have Auto test on, every 6 hours.
- It refreshes OAuth tokens shortly before they expire.
- It flushes drift observations to the database every 30 seconds.
