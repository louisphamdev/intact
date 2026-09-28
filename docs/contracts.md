# Contract Lab

Contract Lab evaluates exchanges between client tools and language model providers.
It finds fields that a converter loses in either direction.
It records schema drift and serves findings and fixtures to test runners.

## Overview

When a client tool sends a request with an `X-Intact-Trace` header, intact captures the exchange.
A full-buffer tap collects the request and the upstream answer up to 4 MiB each.
After the exchange finishes, the upstream converter submits its recorded half.
intact reduces both halves to structural shape records and executes a diff.

## Privacy and Storage

intact does not store raw prompt text or answer text.
From llm-switcher 1.1.5, the half that the switcher uploads carries no client content either: the switcher replaces every string value with `x` of the same length, except the enum values that this reducer reads (`type`, `role`, `object`, `model`, `status`, `event`, `finish_reason`, `stop_reason`). Keys, numbers and flags stay; keys under user data are masked, as the reducer collapses them anyway. As a result, intact matches a field that the converter moved by its path or by an approved mapping, not by its value.
Every string value is reduced to its byte length and a truncated HMAC-SHA256 hash.
The HMAC key is unique to the server and never leaves the host.
Known enum values on an allowlist keep their string value.
All object keys that contain special characters become `{*}`.

## Trace Lifecycle

A trace moves through the following states:

1. `capturing`: The proxy records the request and upstream response.
2. `open`: The proxy seals the trace and waits for the client half.
3. `reducing`: The server converts the raw payload into a shape record.
4. `queued`: The consumer queue accepts the sealed trace.
5. `done`: The consumer finishes the diff and updates findings.

A trace can also end in a terminal state: `expired`, `lost`, `failed`, `dropped`, or `revoked`.

## Trust Model

Only trusted callers can write to shared contract state.
A trusted caller is the web session, the master token, or an API key marked as trusted.
Untrusted traces produce records for their own trace only.
They do not update learned contracts, do not open findings, and do not call the judge.
When a key loses trusted status, open traces for that key move to `revoked`.

## Diff and Findings

The diff compares input leaves against output leaves in both directions.
Each rule runs over all input leaves before the next rule starts:

1. The same path.
2. An approved mapping.
3. The same hash (values of 8 bytes or more).
4. The same path under one renamed segment. For example, `tools[].input_schema.x` matches `tools[].function.parameters.x`.
5. The same enum value on any path.

The enum rule is last, because it must not take the counterpart of a field that a stricter rule matches.
Unmatched leaves become candidates, with two exceptions:
- An empty array or object carries no value, so a converter that drops it loses nothing.
- The reducer cuts paths deeper than 12 levels. If the output was cut at a field, the absence of the field on the output side proves nothing.

The judge evaluates new candidate signatures from trusted traces.
Verdicts for `noise`, `normalized`, and `renamed` stay `proposed` until the owner approves them.
A `lost` verdict or an approved `renamed` verdict opens or updates a finding.

## Automatic review of findings

The drift review also judges each open finding.
It uses the same settings: the switch, the decision model, the resolver model, and the confidence to act alone.
See [Drift](drift.md#automatic-review).

1. intact sends the model, the client format, the direction, the path, and these facts:
   - whether the path lies in a tool definition schema;
   - whether the path lies in user data (`{*}`, `#json`, tool arguments);
   - how many times the field was lost;
   - how many traces carried the field to the other side.
2. The decision model chooses the cause:

   | Cause | Meaning |
   | --- | --- |
   | `dropped_by_design` | The target format has no place for the field, so the converter must drop it. |
   | `carried_in_another_form` | The converter moves the value to another path or another form, so a match by path fails. |
   | `data_noise` | The path lies in user data, tool arguments, or free-form keys. |
   | `real_loss` | The target format has a place for the value, and the converter loses it. |

3. A benign cause (the first three) with the confidence to act alone closes the finding as `wontfix`.
4. The resolver gets every other finding, with the leaning of the decision model.
   Its action is final: `close` or `keep_open`.
   A `close` closes the finding only when the cause is benign.
5. Without a resolver, the other findings stay open with the verdict and the reason.
   When you set a resolver later, it gets these findings.

The history of a closed finding shows `review:<model>` and the reason.
A newer switcher version does not open a finding that the review closed, because the diff is the same.
A finding that opens again for another reason, for example after `expired`, gets a new review.
Only `real_loss` findings stay open for a person.

## Change Detection and Sampling

intact monitors trusted, untruncated records for provider changes.
A new path, a path missing in 20 consecutive traces, or a new event marks the model as changed.
When a change occurs, the sampling rate increases to 100 percent for the next 20 requests.
The server limits change alerts to one notification per tool or model every six hours.
Alert messages contain only a count and a finding identifier.
