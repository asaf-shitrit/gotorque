# 0016. A regression rejects only when it is significant, after a second series if needed

- Status: accepted
- Date: 2026-09-22

## Context

Acceptance needs a statistically supported improvement, but a regression rejected a candidate on
the point estimate alone: any eligible reading (a representative workload, or the pooled primary
metric) whose 25-pair mean moved past `maximum_guardrail_regression_percent` ended it. Short CLI
runs carry bursts of slow outliers that move a mean several percent while the median stays put,
so noise alone rejected candidates. On gron campaign #4 a bufio patch that improved
flatten-users by 20.4% with support was rejected because small-doc read +4.01%, which benchstat
did not find significant.

`StatisticallyFit` could not serve as the test: it is also granted to a flat reading whose
interval rules out a 2% regression, so for a manifest with a tighter limit it would mean the
opposite of a regression.

## Decision

Every comparison carries `Significant`: benchstat's p below 0.05 when it ran, otherwise Welch's
|t| above 2.2. When the manifest asks for statistical support, an eligible reading over the limit
rejects only when it is significant. When one is over the limit without significance, the engine
first measures every representative workload again with a second series of 25 pairs and derives
every comparison from both series. A reading that stays insignificant does not reject, and the
verdict names it. A manifest with `statistical_support_required: false` keeps the point estimate
as the whole rule. The pooled guardrails (`cpu_time_ns`, `peak_memory_bytes`,
`binary_size_bytes`) are unchanged.

## Evidence

A/A trials on gron, the same binary measured against itself, which no rule should reject:

| Run | Old rule (point estimate) | Significance only | This decision |
| --- | --- | --- | --- |
| Earlier, 30 trials, representative seeds | 4 rejected (13%) | 0 | not run |
| Engine and sandbox, 30 trials | 12 rejected (40%) | — | 1 rejected; 10 confirmed |

In the second run the old rule rejected on readings from +2.00% to +9.10%. The confirmation
series brought most of them back toward zero (+2.04% to -0.01%, +2.79% to +1.22%). The one rejection left
was a small-doc reading of +7.54% that benchstat itself called significant, the false-positive
rate a 0.05 test accepts by design. No trial was accepted.

The three decided candidates of the live Jev campaigns:

| Candidate | Deciding readings | Old | New |
| --- | --- | --- | --- |
| #3 attempt 2 | small-doc +3.35%, not significant | rejected | inconclusive |
| #4 attempt 1 | flatten-users -20.41% supported; small-doc +4.01%, not significant | rejected | accepted, unless the second series makes small-doc significant |
| #4 attempt 2 | small-doc +5.87%, significant | rejected | rejected |

## Consequences

A candidate with an unresolved reading costs one more measurement pass, about as long as the
first; on gron that happened in a third of A/A trials and took seconds. The second series has no
budget of its own: it is part of measurement, and the campaign's deadline bounds it as it bounds
the first. Every comparison is then derived from 50 pairs, so the improvement side is judged on
more samples too. A real regression below what 50 pairs can resolve passes with its reading named
in the verdict. Before this, the same reading rejected a candidate a noisy run could equally have
produced.

## Alternatives considered

- Significance without a second series: the same A/A result for less work, but a real regression
  that 25 pairs cannot resolve would pass with nothing done to find it.
- Compare medians: halved the earlier A/A rejections rather than removing them, and doing it
  properly needs a rank-based test on the acceptance side as well.
- Measure again on every over-limit reading, significant or not: a significant first series is
  already the evidence the verdict asks for, and repeating it would only trade false rejections
  for measurement time at the same 0.05 level.
