# Models

A provider's page ends with its model table, with these columns:
- a selection box;
- the model's on/off switch;
- the model's name (click it to copy `<provider>/<model>`);
- its tags (`thinking`, `paid plan`);
- its effort levels;
- its last test result;
- a Test button.

## Fetching the list

intact reads each provider's model list in three situations:

| When | What happens |
| --- | --- |
| On demand | **Fetch now** reads the list and reports `Fetched N models · x new · y removed`. |
| Every hour | Background, for each provider with an active account. A failed fetch is retried within ten minutes. |
| When a request needs it | The cached list serves for ten minutes. |

When a fetch succeeds:
- A model the provider no longer lists is deleted.
- A new model is added: on by default, or as the provider's policy decides
  (below).

A failed fetch deletes nothing. When a provider's list cannot be read, the
provider's fixed fallback list (Claude, Cloudflare) stands in, and it deletes
nothing either.

The last list answer, as the provider sent it, is at
`GET /api/providers/{id}/models/raw`.

## Switches

The per-row switch turns a model on or off. A model that is off is gone from
`/v1/models`, is not picked for a bare id, and a call naming it gets `403`. A
dashboard test can still reach it.

To act on many models:
1. Tick rows. The header box ticks every row shown, and is half-ticked when some are.
2. Use the bar that appears:
   - **Turn on** / **Turn off**;
   - **Keep only these on**: turns the selection on and every other model of
     the provider off, after a confirmation;
   - **Test**: three at a time.

Changing the filter drops hidden rows from the selection, so an action never
reaches a row you cannot see.

## Sorting and filtering

Every table in the dashboard sorts and filters the same way: the model table,
Usage, and Drift → Fields.

- **Sort:** click a column name. It cycles ascending, descending, then off.
- **Filter:** each column has a filter under its name.

  | Column | Filter |
  | --- | --- |
  | text | typed words (the syntax below) |
  | number | a comparison: `>1000`, `<=5`, `10-20`, `=3` |
  | chip | a list of the values present, each with its row count; tick the ones to show (a row with none shows as `—`) |

  **Clear filters** resets them all. On the model table, **Quick filter**
  offers `free` and `thinking`.

Text filters read words like this:

| Filter | Matches |
| --- | --- |
| `free mini` | names containing both words |
| `free\|beta` | names containing either |
| `-preview` | excludes names containing it |
| `/regex/` | a regular expression |

On the Usage page:
- The Day filter starts on today when there is usage today.
- The totals above the table count only the rows shown.

## Policy: Auto test and Only free

Two switches above the table set how a provider's models are switched without
you.

**Only free** turns on only the models whose name contains `free`, such as
OpenRouter's `:free` models. A new model starts on only when free. Turning Only
free off switches every model back on.

**Auto test** turns a model on only when a test call works:
1. When switched on (after a confirmation), it fetches the list and tests every
   model in the background. Under Only free, it tests only the free models, and
   the others go off untested.
2. A model that answers goes on. A model that fails goes off, rate limits
   included.
   - A model still rate-limited after one retry is tested again 30 minutes later.
3. It runs again every 6 hours. A new model found by a fetch is tested before it
   goes on.
4. Under Auto test, pressing **Test** on a row also sets that row's switch.

Changing either switch stops a run in progress before the new policy applies,
so a run under the old policy cannot undo it. Tests use the provider's quota:
each is a short call (up to 64 tokens).

The policy is stored per provider. It is available through
`/api/providers/{id}/model-policy` and the MCP tool `set_model_policy`.

## Tests

| Test | How |
| --- | --- |
| Row **Test** | A short chat request through the same `/v1` path a client uses: translation, blacklist and failover included. The result names the account that answered. |
| Account **Test** | The same call pinned to one account, even one that is off. It uses the provider's model that last passed a test, else its first model that is on. |
| TypeSafe | Jev does not chat. Its test is one `/v1/systemone` call about a support message whose answers are known: urgent → yes, bug → no, team → billing, customer upset → above calm. It checks the shape of each typed answer and that it is right. |

Results are kept with the model or the account.

## Chips and the Effort column

What each provider's own list says about a model:

| Mark | Meaning |
| --- | --- |
| `thinking` | The model can think (reason before answering). |
| `thinking · always` | It always thinks; thinking cannot be switched off. |
| `paid plan` | Cloudflare serves it only on the paid Workers plan. |
| Effort chips | The levels a request may ask for. They are coloured from cool (`none`, `low`) to hot (`high`, `xhigh`, `max`, `ultra`); the default level is filled. |

Where each provider states thinking support:

| Provider | Where it is stated |
| --- | --- |
| Claude | `capabilities.thinking`, `capabilities.effort` |
| Codex | `supported_reasoning_levels`, `default_reasoning_level` |
| Copilot | `capabilities.supports.reasoning_effort`, `adaptive_thinking`, `max_thinking_budget` |
| OpenRouter | `supported_parameters` (`reasoning`, `reasoning_effort`), `reasoning.mandatory` |
| Groq | `supported_features` (`reasoning`) |
| Cloudflare | `properties` (`reasoning`, `reasoning_effort`, `require_workers_paid`) |
| Antigravity | `supportsThinking`; the levels are its folded variants |

NVIDIA, TokenHarbor-style custom endpoints and TypeSafe say nothing, so their
models carry no chip.

## Rankings

The **Tier** and **Arena** columns say how strong a model is. They come from
[LMArena](https://lmarena.ai)'s public leaderboard: an Elo-style rating from
people voting between two anonymous answers. The data is the official
`lmarena-ai/leaderboard-dataset` (CC-BY-4.0).

intact reads three boards, **Overall**, **Coding** and **WebDev**:
- It reads them once a day and keeps them in the database.
- The switch above the table picks which board the columns show.
- The Arena cell shows the rating and the rank. The tooltip gives every board
  with its votes.

The Tier grades a model by how far it is below the board's best model. A gap
of 100 points means the stronger model wins about 64% of votes.

| Tier | Below the top model |
| --- | --- |
| S | ≤ 25 points |
| A | ≤ 70 points |
| B | ≤ 130 points |
| C | ≤ 250 points |
| D | more |

**Matching.** A provider's model id is matched to a board name after both are
normalised:
- the vendor path and `:free`-style tags are dropped (`openai/`, `@cf/…/`);
- dots and underscores become dashes (`4.6` = `4-6`);
- date and build suffixes are dropped (`-20251001`, `-instruct`, `-fp8`,
  `-preview`…).

When the board lists the model only with an effort (`-high`, `-max`,
`-thinking`…), the most-voted of those is used. A name shown under the rating is
the board entry the model was matched to, when it is not the same name.

**Fixing a match.** Click the Arena cell to set the board name by hand. You can
also choose **Automatic** to match by name again, or **Not on the board** to
show no rating. Models the board does not know (embeddings, speech, guards,
internal models) show `?`.

## Antigravity variants

Antigravity's per-level models are shown as one row, and their levels appear in
the Effort column. The default is `tiered`, used when a request names no
effort. See [Routing](routing.md#antigravity-level-variants) for how the level
is picked.
