# Alerts

intact tells the channels you declare what it did on its own, and what it
could not fix. Channels are set on the **Alerts** page, at `/api/notify`, or
with MCP.

## Channels

A channel is a type and the fields of that type. The form shows the fields
of the type you pick; the server describes them (`GET /api/notify` returns
`types`), so a new kind of channel is one entry in `notifyTypes` in
`internal/httpapi/notify.go`.

| Type | Fields |
| --- | --- |
| `telegram` | bot token (from @BotFather; the bot must be in the chat), chat id, topic id (a forum group's `message_thread_id`; empty for the general topic), dashboard address |
| `webhook` | URL, bearer token (optional), dashboard address |

A webhook receives a JSON POST: `{"event","title","lines","at","link"}`.

The dashboard address, when set, adds a link to the page the alert is about.

Secrets:
- They are never returned. The API shows `••••` and the last four characters.
- Saving a channel with a secret left empty (or masked) keeps the stored one.

**Test** sends a test alert. Each channel shows when it last sent and the last
error, if any.

## Events

A channel takes the events ticked for it; none ticked means every event.

| Event | When | Repeats |
| --- | --- | --- |
| `error.action` | The [error review](errors.md#automatic-review) blacklisted a rule, or switched off a model or an account. | each time |
| `error.unresolved` | The error review proposed an action that intact refused, so the group is not fixed. | once a day per group |
| `error.burst` | 20 or more errors of one group in an hour. | once in 6 hours per group |
| `drift.action` | The [drift review](drift.md#automatic-review) blacklisted a request field. | each time |
| `review.paused` | The drift or error review cannot reach its model and waits 10 minutes. | once in 6 hours |
| `account.auth` | An account's token could not be renewed: sign in again. | once in 6 hours per account |

## API and MCP

| Route | Meaning |
| --- | --- |
| `GET /api/notify` | Channels (secrets masked), `types` with their fields, `events`. |
| `POST /api/notify/channels` | Create: `{"name","type","enabled","events":[…],"config":{…}}`. |
| `PUT /api/notify/channels/{id}` | Replace. |
| `DELETE /api/notify/channels/{id}` | Delete. |
| `POST /api/notify/channels/{id}/test` | Send a test alert. |

MCP tools: `list_notify_channels`, `put_notify_channel`,
`test_notify_channel`, `delete_notify_channel`.

## This deployment (2026-09-22)

Alerts go to the topic **🛡 intact alerts** of the Hermes News Telegram group,
through Hermes' bot. The owner asked for one place where every problem
reaches them; later users pick their own channels on this page.
