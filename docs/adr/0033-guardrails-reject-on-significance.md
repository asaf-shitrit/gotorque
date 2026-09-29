# 0033. Guardrails reject on significance, like workload regressions

- Status: accepted
- Date: 2026-09-29

## Context

ADR 0016 made an acceptance-eligible reading reject only when it regresses past the limit
significantly, with a second series of pairs first when it does not. Guardrails (CPU time,
peak memory, binary size) kept a plain threshold: any point estimate over the limit rejected.
That choice came from a real failure. A version that demanded statistical support of a
guardrail reported a candidate inconclusive because its memory moved +0.16% without
significance, which blocked a supported -14.38% win.

The first null-candidate run (#40) measured the cost of the threshold. Twenty candidates on gron,
each adding one comment line and leaving the compiled code identical, produced no accepts and
two rejections. Both were `cpu_time_ns` guardrail readings of +2.16% and +3.06% that were not
significant, taken while another session's browser tests pushed the load to 5-6.6 on a
10-core machine. That is a false-rejection rate of 2 in 16 valid runs, about 12%, on a bound of
four attempts per campaign.

## Decision

When the manifest requires statistical support, a guardrail past its limit rejects only when the
difference is significant. An insignificant over-limit reading:

- triggers the same second series that ADR 0016 gives eligible readings
  (`policy.UnconfirmedGuardrails`, consumed by `confirmRegressions`);
- if it stays insignificant, is named in the verdict's reasons, and neither rejects nor makes the
  candidate inconclusive.

A guardrail within its limit still needs no support. That was the old failure, and it is
unchanged. With `statistical_support_required: false`, the limit alone decides, as before.

## Consequences

- A real regression still rejects. The significance test counts a consistent difference with no
  spread as significant, and 25 to 50 pairs resolve a steady +2-3% CPU or memory increase.
  Binary size compares exact sizes, so any growth past the limit is significant.
- A noisy regression near the limit can now pass when two series cannot resolve it. That is the
  same trade ADR 0016 made for workload readings.
- The null-candidate run is the check: repeated on gron and other targets, it should show no
  guardrail rejections of comment-only patches.
