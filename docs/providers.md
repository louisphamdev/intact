# Providers

The **Providers** page groups providers in three sections:
- **OAuth sign-ins**: coding tools, added by signing in.
- **API providers**: known endpoints, added by pasting a key.
- **Declared providers**: any other provider, declared with its URL and
  sign-in (see [Declared providers](#declared-providers)).

The first two sections hide providers with no account. **Add** opens a list of
every provider, split into connected and not yet connected.

Each account has:
- a name, editable in place;
- an on/off switch: an account that is off is skipped by `/v1`;
- a **Test** button: one short call through that account alone, with its
  result kept beside the account;
- a Delete button.

## Built-in providers

| Id | Provider | Credential | Upstream shape | Model list | Quota |
| --- | --- | --- | --- | --- | --- |
| `claude` | Claude Code (Anthropic) | OAuth sign-in | Anthropic | `/v1/models`; fixed list when unreadable | API |
| `codex` | Codex (ChatGPT) | OAuth sign-in | Responses, always streamed | `/models?client_version=…` | API |
| `antigravity` | Antigravity (Google) | OAuth sign-in | Gemini in the Antigravity envelope | `fetchAvailableModels` | API |
| `github` | GitHub Copilot | GitHub device sign-in | OpenAI; Anthropic for Claude models; Responses for some | `/models` (chat models the plan enables) | API |
| `groq` | Groq | API key | OpenAI | `/models` | API + headers |
| `nvidia` | NVIDIA NIM | API key | OpenAI | `/models` | — |
| `openrouter` | OpenRouter | API key | OpenAI | `/models` | API |
| `cloudflare-ai` | Cloudflare Workers AI | API token + account id | OpenAI | model search (text generation); fixed list when unreadable | — |
| `typesafe` | TypeSafe (System One) | API key | its own (`/v1/systemone`) | `/models` | — |

"Quota: headers" means intact reads the rate-limit headers of the last answer.
See [Quota and usage](quota-and-usage.md).

### Notes per provider

- **Claude Code.** intact sends the recorded Claude Code headers. Tokens refresh
  with the stored refresh token.
- **Codex.** Calls go to `chatgpt.com/backend-api/codex` with the ChatGPT account
  id read from the token. A client may call `/v1/responses` with a Codex model
  and get Codex's own stream byte for byte.
- **Antigravity.**
  - intact loads the account's project on first use and keeps it.
  - Level variants are folded (see [Routing](routing.md#antigravity-level-variants)).
  - The Claude Code billing line is blacklisted by default: Google answers a
    false 429 when it is present.
- **GitHub Copilot.**
  - The stored GitHub token is exchanged for a short-lived Copilot token,
    cached and exchanged again on 401.
  - For gpt-5 and o-series models, `max_tokens` is renamed `max_completion_tokens`.
- **Cloudflare.**
  - The account id is part of the base URL and is asked for when you add the
    account.
  - The model search also says which models only the paid Workers plan serves.
- **TypeSafe.**
  - Jev is a decision model: a request sends `state` and typed `questions`
    (noul, choice, score) to `/v1/systemone`, and gets typed `answers` back.
  - The Endpoint page has a TypeSafe tab with a sample. Its test is its own
    (see [Models](models.md#tests)).

## Sign-in flows

A provider's own sign-in page redirects only to the address its client
registered, usually `localhost`. So intact cannot catch the redirect on its own
domain. Each flow therefore ends with you pasting what the page gave you.

| Provider | Flow | What you paste |
| --- | --- | --- |
| Claude Code | PKCE; the page is Anthropic's code page | the `code#state` string it shows |
| Codex | PKCE; redirects to `http://localhost:1455/auth/callback` | the full redirect URL from the address bar (the page itself fails to load, which is expected) |
| Antigravity | PKCE; redirects to `http://localhost:51121/oauth-callback` | the full redirect URL; needs `INTACT_ANTIGRAVITY_CLIENT_SECRET` |
| GitHub Copilot | Device flow | nothing: open the link, type the code shown, and intact polls until you approve |

The steps:
1. **Login** asks for a name for the account.
2. It opens the provider's page.
3. You paste the result into the box.

A pending sign-in is stored in the database for 30 minutes, so a restart of
intact in between does not lose it. The state is checked before the pending
sign-in is used.

## Declared providers

Any provider can be added without code: **Providers → Declared providers →
Declare a provider**, or the API and MCP. A declared provider works like a
built-in one:
- routing by its id (`<id>/<model>`);
- the model table and tests;
- rotation, the blacklist and usage.

The form asks for the fields of its **kind**:

| Kind | Accounts | Fields |
| --- | --- | --- |
| `apikey` | a pasted key or token | id, name, API standard, base URL, auth header and prefix, extra headers, model list URL or fixed models, colour, icon |
| `oauth-code` | sign in in the browser, paste back the address it ends on (or the code) | as `apikey`, plus authorize URL, redirect URI, token URL, client id and secret, scope, extra authorize parameters, PKCE on or off |
| `oauth-device` | enter a device code on the provider's page; intact polls | as `apikey`, plus device code URL, token URL, client id and secret, scope, verification page (when the provider names none) |

Defaults and conventions:
- **API standard:**
  - `openai` calls `<base>/chat/completions`;
  - `anthropic` calls `<base>/messages` and sends `anthropic-version`;
  - `responses` calls `<base>/responses`.
- **Credential header:** empty means `Authorization: Bearer <key>`. An
  Anthropic API key uses `x-api-key` instead.
- **Model list:** empty means `<base>/models`. The list is read whether it is
  wrapped (`{"data":[…]}`) or a bare array, and a `models/` prefix on ids
  (Google) is dropped. With `none`, the declared models are the list.
- **Account id:** `{accountId}` in the base URL (or the model list URL) makes a
  new account ask for its account id.
- **OAuth tokens** are refreshed with the declared token URL, client id and
  secret. The provider must use the standard `refresh_token` grant.

The **JSON** view of the form takes or gives the whole definition. The
definition card on the provider's page copies it, so a provider declared once
can be pasted into another intact. For example:

```json
{
  "id": "deepseek", "name": "DeepSeek", "kind": "apikey", "api": "openai",
  "baseUrl": "https://api.deepseek.com/v1"
}
```

Other rules:
- **Deleting** a declared provider needs its accounts deleted first.
- **Custom endpoints** made by older versions (a base URL kept on an account)
  are turned into declared providers at start.

## Adding a provider to the registry

Most providers need no code: declare them instead. A provider needs code only
when it has a protocol of its own (an envelope, a token exchange, a special
list). A built-in provider is an entry in `internal/provider/registry.go`:

| Field | Meaning |
| --- | --- |
| `BaseURL` | May contain `{accountId}`. |
| `API` | The shape: `""` for OpenAI, `anthropic`, `responses`, `antigravity` or `typesafe`. |
| `AuthHeader`, `AuthPrefix` | How the credential is sent. |
| `Setup` | How an account is added: `key`, `account`, `oauth` or `none`. |
| `Identity`, `Defaults` | Headers every call carries. |
| `RequestIDHeader`, `AccountHeader`, `SessionHeader` | Headers derived per call. |
| `ModelsPath`, `ModelsQuery` | Where the model list is read. |
| `Models` | A fallback list for when the list cannot be read. |
| `Exchange` | A token exchange (Copilot). |
| `Watch` | Learn its traffic for [drift](drift.md). |

Add an icon as `internal/web/icons/<id>.png`.
