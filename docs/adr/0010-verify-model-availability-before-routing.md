# 0010. Verify a model is available before a campaign spends on it

- Status: accepted
- Date: 2026-09-20

## Context

`stealth/union-alpha` was advertised as free and worked: a live call returned the requested JSON
with `"cost": 0`, and the provider-path check decoded a real coordinator answer in 13.4 s. Routing
every role to it then lost 10 of 13 campaign attempts to the two-minute idle bound, produced no
usable patch, and burned a 20-minute budget. Later calls explain why: `404 Not Found … Thank you for
participating in the Stealth Union Alpha testing period. This model was Unbiased's Pareto. Use it
now: https://openrouter.ai/unbiased/pareto`. The model had been retired, and its successor
`unbiased/pareto` is priced $2.50/M prompt and $7.50/M completion — not free.

## Decision

Keep the opt-in provider-path check (`GOTORQUE_LIVE_MODEL=<slug> go test ./internal/agents -run
TestLiveModelAnswersARolePrompt`) that validates a slug against the endpoint's catalogue and decodes
one real answer, and treat stealth and `:free` slugs as temporary: verify before routing a campaign
to one.

## Consequences

A wrong or retired slug fails with the tool's own message (`configured model "…" for coordinator is
not advertised by endpoint`) or the endpoint's exact text, instead of a confusing decode failure or a
campaign of silent stalls. CI needs no secret because the check skips without one.

## Alternatives considered

Assume availability (rejected: it cost a 20-minute campaign to learn otherwise). Pin the retired
slug (impossible: it no longer exists).
