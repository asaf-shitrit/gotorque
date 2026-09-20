# 0002. A verdict may rest on any acceptance-eligible reading

- Status: accepted
- Date: 2026-09-20

## Context

Acceptance was decided on the pooled average across representative seeds. On gron a candidate
improved its large-document seed by 3.35% with support (benchstat p=0.006) while the 41-byte seed,
whose measured run is mostly process startup and cannot be improved by any patch, sat at -0.81%
unsupported and pulled the pooled figure to -2.53% against a 3% bar. The next round produced the
same shape (-3.67% eligible, -2.02% pooled), and an independent 30-pair measurement of that patch
put the affected workload at -3.75% (p<1e-4). Supported wins were being reported as inconclusive.

## Decision

A candidate passes when any reading of the primary metric — the pooled one or any
representative-tier seed — clears the manifest's threshold with statistical support, and is rejected
when any eligible reading regresses past `maximum_guardrail_regression_percent`.

## Consequences

The verdict names the reading it rests on, so a reader can see which workload carried it. The
pooled figure keeps its meaning as the aggregate and is still what most verdicts quote.

## Alternatives considered

Keep pooled-only (rejected: it refuses supported, reproducible wins). Accept on a per-workload win
without the regression guard (rejected: a win bought by hurting another representative workload is
not a win).
