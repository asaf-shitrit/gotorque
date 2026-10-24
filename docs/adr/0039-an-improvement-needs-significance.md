# 0039. An improvement is supported only when it is significant

- Status: accepted
- Date: 2026-10-24

## Context

`StatisticallyFit` is the field `policy` reads to decide whether a reading is
evidence of a win: `decideOnImprovement` accepts on it, `lookupPrimary` refuses
a verdict without it, and `UnconfirmedImprovements` and `BorderlineImprovements`
select readings by it. It answers "are these samples strong evidence about the
candidate's effect?", and it had two routes to yes: Welch's `|t| > 2.2`, or the
95% interval of the relative delta lying entirely below the 2% guardrail limit.

The second route is an equivalence check written for jittery flat guardrails
(commit 4956071): a peak-memory reading that moved by nothing should not read as
unsupported just because its spread is wide. It was not restricted to readings
that are not improvements, so it also fired for any improvement whose upper
bound fell under 2%, wherever the significance test found nothing. For a -D%
reading every standard error between D/2.2 and (D+2)/2.2 passed, which at the 3%
minimum means `|t|` from about 1.36 to 2.2, or p from 0.03 to 0.2. The bound was
also the literal 2.0 rather than the manifest's limit.

`benchstat` hides it. `benchstat.go` overwrites `StatisticallyFit` with a
parseable p-value when the binary is installed, and CI installs it, so this path
is the one a machine without `benchstat` takes — exactly the machines the README
promises campaigns complete on.

## Decision

`metricSupport` grants equivalence support only to a reading that is not an
improvement: a delta of zero or a regression. An improved reading is supported
when it is significant and not otherwise. The equivalence bound is the metric's
own regression ceiling, read through the new `policy.RegressionLimit`: the
guardrail's `maximum_regression_percent` when the manifest gives it one — which
is where `--tradeoff` and `--allow` land — and the manifest's
`maximum_guardrail_regression_percent` otherwise.

A non-significant improvement past the minimum is now an inconclusive verdict
that `confirmImprovements` measures again (ADR 0021), and the policy then
decides on both series. It is never a borderline accept, because
`BorderlineImprovements` requires support.

## Evidence

The reported series: 25 interleaved pairs on a 100 ms workload, a real -3.50%
reading carrying a shared ±12 ms of per-pair jitter, so the difference is not
detectable (Welch `|t|` 1.3, p about 0.16). Before this, `StatisticallyFit` was
true and `policy.Evaluate` accepted; now `Significant` and `StatisticallyFit`
are both false, the verdict is inconclusive, and fifty pairs of the same
distribution (`|t|` about 1.84) still do not accept. A jittery flat guardrail,
and a 1% binary-size move against go-jsonnet's 5% limit, are still supported.

Recorded verdicts were replayed before the change was kept, the practice ADR 0016
set: 151 campaigns under `~/projects/gotorque-work/campaigns` (522 candidate
records, 420 measured) were re-judged with every improvement-only equivalence
support withdrawn. 250 records carried such a reading, every one of them a
guardrail improvement (`cpu_time_ns`, `peak_memory_bytes`, -0.03% to -4.46%);
none was an acceptance-eligible reading. No verdict changed: accepted,
inconclusive and rejected all stayed as recorded, because policy reads
`StatisticallyFit` only on acceptance-eligible readings and those were never
granted support this way in the record. The replay can therefore neither confirm
nor refute how often a wall-time win was accepted without `benchstat` in
practice; it confirms that tightening the rule removes no real verdict from the
record.

## Consequences

- A win that only the equivalence branch supported is now inconclusive, and
  costs the one confirmation series ADR 0021 already runs. A real win that
  twenty-five pairs could not resolve accepts when both series together resolve
  it.
- A guardrail improvement no longer reads as statistically supported in a
  report. No verdict used that field on a guardrail.
- `metricSupport` and `compareMetric` take the metric's limit, and the evaluator
  resolves it from the campaign's own manifest, so a manifest limit, a trade-off
  preset and an `--allow` all reach the bound with no second copy of the default.
- A machine without `benchstat` no longer accepts an improvement on an interval
  that merely rules out a regression.

## Alternatives considered

- Restrict equivalence support to guardrail metrics by name: a verdict reads the
  primary metric's support too, and a per-workload primary reading that is flat
  can be the eligible reading a verdict rests on. Direction is the property that
  matters, not the metric's role.
- Keep the equivalence route for improvements but tighten its bound to the
  minimum improvement: the bound is a regression ceiling; reusing it as an
  effect-size test would accept a -3% reading on an interval whose upper end is
  still above zero.
- Require `benchstat` for acceptance: it is optional by design, and the harness
  must not need it to be honest.
