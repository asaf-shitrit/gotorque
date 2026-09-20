# 0003. The target manifest's performance block is the acceptance contract

- Status: accepted
- Date: 2026-09-20

## Context

`performance.minimum_improvement_percent`, `maximum_guardrail_regression_percent`,
`primary_metric`, `guardrails` and `statistical_support_required` were loaded, defaulted and
validated, and then never applied: the decision was handed `policy.DefaultConfig()`. A target
declaring a 5% floor was judged by 3%, and a narrower guardrail list was ignored.

## Decision

The policy configuration is built from the target's manifest; the documented defaults fill in only
what a manifest leaves out.

## Consequences

A target's declared criteria take effect. The loader and schema already reject invalid values, so
no new failure mode is introduced.

## Alternatives considered

Keep hardcoded defaults (rejected: it makes the manifest's performance block decorative, which is
what the schema and docs claim is the contract).
