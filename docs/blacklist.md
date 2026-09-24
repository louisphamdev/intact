# Blacklist

A provider that meets a field it does not know often fails the whole request.
The blacklist removes such things just before a request leaves: after
translation, on the exact bytes the provider will receive. A change takes
effect on the next request, with no restart.

## Kinds

| Kind | Pattern example | Removes |
| --- | --- | --- |
| `field` | `messages.*.cache_control` | A key at a dot path of the body. `*` matches any key or list item. |
| `schema` | `$id` | A key at every depth of each tool's JSON schema. A tool parameter with that name is kept. |
| `system` | `^x-anthropic-billing-header:.*$` | Lines of the system prompt matching the regular expression. |
| `header` | `anthropic-beta` | A request header, including a provider default. The credential header is never removed. |

## Scope

A rule applies to one provider (its id) or to every provider (`*`). A rule can be
switched off without deleting it.

## Seeded rules

On first start the blacklist is seeded with fixes found against real providers:
- tool-schema keys `encrypted`, `cache_control`, `$id` and `example`, which
  strict schema validators refuse;
- the Claude Code billing line, for Antigravity: its presence in the system
  prompt makes Google answer 429 while quota remains.

## Rules found in production

The error review found these rules on a live install and proved each one by
replay. A new install does not have them. Add the rules for the providers that
you use, from the dashboard or with the API below.

| Provider | Kind | Pattern | Why |
| --- | --- | --- | --- |
| `antigravity` | `system` | `\bYou\s+are\s+Codex,\s+an\b` | Codex system prompt. Google answers a false 429 to "You are Codex, an agent based on GPT-5". |
| `antigravity` | `system` | `\bYou\s+are\s+Codex,\s+a\b` | The same fingerprint in the variant "You are Codex, a coding agent based on GPT-5". |
| `antigravity` | `system` | `\ba\s+Claude\s+agent,` | Claude Code system prompt (`claude -p`). Google answers a false 429 to "You are a Claude agent, built on Anthropic's Claude Agent SDK". |

The rule removes only the matched words. The rest of the system prompt stays.

To add all three at once:

```bash
for p in '\bYou\s+are\s+Codex,\s+an\b' '\bYou\s+are\s+Codex,\s+a\b' '\ba\s+Claude\s+agent,'; do
  jq -n --arg p "$p" '{provider:"antigravity",kind:"system",pattern:$p,note:"false 429: client fingerprint",enabled:true}' |
    curl -s https://intact.example/api/filters -H "Authorization: Bearer $INTACT_API_TOKEN" \
      -H "Content-Type: application/json" -d @-
done
```

`INTACT_API_TOKEN` is the master token: only an admin can add a rule.

If Google refuses a new fingerprint later, the error review finds it and adds
the rule by itself. See [Errors](errors.md).

## Managing rules

- **Dashboard**: **Blacklist** has one tab per kind. **Block** adds a rule.
- **API**:

  ```bash
  curl https://intact.example/api/filters -H "Authorization: Bearer $INTACT_KEY" \
    -d '{"provider":"groq","kind":"field","pattern":"reasoning_effort","note":"groq rejects it"}'
  curl -X DELETE https://intact.example/api/filters/<id> -H "Authorization: Bearer $INTACT_KEY"
  ```

  A rule has the fields `id`, `provider`, `kind`, `pattern`, `note` and
  `enabled`. Posting with an `id` updates that rule.
- **MCP**: `list_filters`, `add_filter`, `update_filter`, `delete_filter`.

## Deciding what to block

[Drift](drift.md) records when a client starts sending a field it did not send
before. That is the usual moment a provider starts refusing requests, and the
change record shows the path to block.

### Reasoning effort on non-reasoning targets

When intact translates an Anthropic Messages request containing extended
thinking to an OpenAI-compatible target, it maps the thinking budget to
`reasoning_effort`. Non-reasoning OpenAI targets (such as Groq) reject this
field with an error. To fix this, add a per-provider field filter for
`reasoning_effort`:

```bash
curl https://intact.example/api/filters -H "Authorization: Bearer $INTACT_KEY" \
  -d '{"provider":"groq","kind":"field","pattern":"reasoning_effort","note":"groq rejects reasoning_effort"}'
```
