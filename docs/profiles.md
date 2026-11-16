# Profiles

A profile is a named group of one provider's accounts. A call that names the
profile is served by the group's members alone; every other call of the
provider keeps using all of its accounts. A profile splits accounts off for
some callers without moving them away from the others.

## Calling through a profile

| `model` in the body | Served by |
| --- | --- |
| `antigravity/gemini-3.8-flash` | Every active account of `antigravity`. |
| `antigravity-lite/gemini-3.8-flash` | The members of `antigravity`'s profile `lite` alone. |
| `antigravity-lite` alone in `GET /v1/models/<id>` | The provider's model list, under the profile's prefix. |

The prefix is `<provider>-<profile>`. A real provider's prefix wins: if a
provider id `groq-lite` exists, `groq-lite/<model>` reaches that provider, and
intact refuses to create a profile that would build such a prefix.

Within a profile, the accounts follow the provider's rotation (its mode,
sticky and order), but they take their own turns: the profile's cursor is
separate from the provider's own. A session keeps its affinity. A standby
member still serves last inside its profile, and a profile never reaches an
account that is not its member — not even the provider's standby.

## Managing profiles

Read is any token; the writes need the dashboard session or the master token.

| Method and path | What it does |
| --- | --- |
| `GET /api/profiles` | Lists the profiles. |
| `POST /api/profiles` | Creates one: `{"provider":"antigravity","name":"lite","members":["<connection id>", …]}`. |
| `PUT /api/profiles/{id}` | Renames it, moves its members, or both; fields left out keep their value. |
| `DELETE /api/profiles/{id}` | Removes the group. The accounts stay. |

The dashboard serves the same four under `/profiles` (its writes by form).

A member that is deleted or switched off just stops serving; adding it back
restores it. Members are connection ids, in the order they were added; the
provider's rotation order reorders them as it does everywhere else.

A name is lowercase letters, digits or `-`, up to 40. Two profiles of one
provider cannot share a name.
