# Security policy

## Report a vulnerability

Do not open a public issue for a security fault.

Report it through GitHub private advisories: open the **Security** tab of this
repository, then select **Report a vulnerability**. The report stays private
until a fix is published.

Include in the report:

- what the fault lets an attacker do;
- the steps that show it;
- the commit or release you tested.

You get an answer in 7 days. A fix goes into a new release, and the advisory is
published with it.

## Supported versions

The latest commit on the default branch gets fixes. Older tags do not.

## Scope

intact holds provider credentials, so these faults are the most serious:

- a route that gives a credential to a caller;
- a way past the TOTP sign-in or the API key gate;
- a way for a dashboard key to reach an admin route.

The limits of the design are written in `docs/security.md`. A report that names
a documented limit is a feature request, not a vulnerability.
