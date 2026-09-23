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
| `-insecure-no-auth` | | Start with no sign-in gate. Every caller then reaches every stored credential. |

intact refuses to start when `INTACT_TOTP_SECRET` is not set. A loopback bind
does not protect the server, because the reference deployment sends tunnel
traffic to that same loopback port. If you accept an ungated server, pass
`-insecure-no-auth` or set `INTACT_INSECURE_NO_AUTH=1`. With no gate, intact
answers a loopback `Host` header only.

## Environment

| Variable | Meaning |
| --- | --- |
| `INTACT_TOTP_SECRET` | Base32 TOTP secret for the dashboard sign-in. intact does not start when it is empty. |
| `INTACT_INSECURE_NO_AUTH` | Set it to `1` to start with no sign-in gate. It does the same as `-insecure-no-auth`. |
| `INTACT_API_TOKEN` | The master token. A machine sends it to `/v1`, `/api` and `/mcp`, and it reaches every route. A key made in the dashboard reaches less: see [Security](security.md#what-a-dashboard-key-cannot-do). Optional. |
| `INTACT_SESSION_KEY` | Key that signs the session cookie. When empty, a random key is made at start, so a restart ends every session. |
| `INTACT_SESSION_TTL` | Session lifetime in seconds (default 43200). |
| `INTACT_OWNER_LOOPBACK` | `1` gives a loopback sign-in with no forwarding header an unlimited budget. Off by default. Read `docs/security.md` before you set it. |
| `INTACT_ARENA_URL` | A mirror of the Hugging Face datasets server `/rows` API for the LMArena rankings (default `https://datasets-server.huggingface.co`). |
| `INTACT_ANTIGRAVITY_CLIENT_SECRET` | Optional. Overrides the Google OAuth client secret of the Antigravity sign-in. intact ships the secret of the Antigravity app, so you do not need the Antigravity CLI or this variable. |

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
UMask=0077
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
   miss what is still in the write-ahead log. Create a mode 700 backup directory
   and run the backup under umask 077:

   ```bash
   install -d -m 0700 /opt/intact/backup
   (umask 077 && sqlite3 /opt/intact/intact.db ".backup /opt/intact/backup/intact-$(date +%F-%H%M).db")
   ```

2. Keep the old binary for a rollback (`intact.prev`), install the new one, and
   restart: `systemctl restart intact`.

Schema changes are applied at start and are additive.

## Background work

After start, intact runs these jobs on its own:
- It fetches each provider's model list every hour and deletes models the
  provider dropped. See [Models](models.md#fetching-the-list).
- It re-tests providers that have Auto test on, every 6 hours.
- It reads the LMArena rankings once a day.
- It refreshes OAuth tokens shortly before they expire.
- It flushes drift observations to the database every 30 seconds.
