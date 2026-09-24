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
It first matches on the same path, then on approved mappings, then on matching hashes.
Unmatched leaves become candidates.
The judge evaluates new candidate signatures from trusted traces.
Verdicts for `noise`, `normalized`, and `renamed` stay `proposed` until the owner approves them.
A `lost` verdict or an approved `renamed` verdict opens or updates a finding.

## Change Detection and Sampling

intact monitors trusted, untruncated records for provider changes.
A new path, a path missing in 20 consecutive traces, or a new event marks the model as changed.
When a change occurs, the sampling rate increases to 100 percent for the next 20 requests.
The server limits change alerts to one notification per tool or model every six hours.
Alert messages contain only a count and a finding identifier.
