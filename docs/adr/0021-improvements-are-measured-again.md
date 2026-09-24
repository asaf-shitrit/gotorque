# 0021. A promising improvement is measured again, mirroring a regression

- Status: accepted
- Date: 2026-09-25

## Context

ADR 0016 gave an unresolved regression a second series before letting it reject a candidate: a
short CLI workload's noise can carry a reading past the guardrail limit without the effect being
real, and twenty-five pairs often cannot tell the two apart. The same noise floor works against
the acceptance side and nothing answered it there. On live yq campaigns (~20ms workloads, 25
interleaved A/B pairs) three candidates read per-workload improvements of 11.55%, 9.51% and 5.39%
that were not statistically supported. None of them regressed anything, none failed a test, and
each ended inconclusive and was discarded — not because the effect was not real, but because
twenty-five pairs cannot separate a 5-10% change from noise at that duration. ADR 0004 sized the
pair count against exactly this noise floor and it is the same floor here; the fix is the same
kind ADR 0016 already applied to the other side of the same threshold.

## Decision

After the first measurement series, and again after a regression confirmation series if one ran,
the engine checks whether the candidate would end inconclusive only because an acceptance-eligible
reading of the primary metric improved by at least `minimum_improvement_percent` but lacks
statistical support, with nothing about to reject it (no eligible reading regressed significantly
past its limit, and behavior still matches). When that is so, every representative workload is
measured again over a second series of the same size and every comparison is derived again from
both series, exactly as `confirmRegressions` already does — same `runSeries`, same `rederive`, same
behavior check. The unchanged policy then decides on the combined fifty pairs.

`internal/policy.UnconfirmedImprovements(config, eligible)` is the predicate: the eligible readings
that cleared the minimum improvement but are not `StatisticallyFit`. It mirrors
`UnconfirmedRegressions` in shape and in role — the engine measures again on exactly the readings
the policy itself would otherwise leave stranded — and returns nothing when the manifest does not
ask for statistical support, since the point estimate then decides alone.

At most one extra series runs per candidate in total. If the regression confirmation already
extended every seed, the improvement check reuses that series (detected by the
`interleaved-ab-confirmation` validation job already being present) instead of running a third.
The event this records is `improvement_confirmed`, and the evidence summary gets a note naming
every reading that triggered it, the same shape `confirmationNote` produces for a regression.

## Evidence

The three yq readings above are the motivating case: 11.55%, 9.51% and 5.39% improvements, each
comfortably clear of a typical 3% minimum, each discarded for lack of significance at 25 pairs.
ADR 0004 measured a 3.75% win going from unsupported at seven pairs to significant at p<1e-4 with
thirty; a fifty-pair series is well inside the range that has resolved comparable effects before.

## Consequences

A candidate with a promising but unsupported improvement costs one more measurement pass, the same
size and cost as the first — seconds for a short CLI workload, bounded by the campaign's deadline
exactly as the first series and the regression confirmation are. Every comparison is then judged on
one hundred combined pairs when both confirmations run, or fifty when only one does, so both sides
of the verdict get the same statistical treatment. A real effect that fifty pairs still cannot
resolve stays inconclusive, now with more evidence behind that conclusion rather than less.
`internal/policy` gains one pure predicate and no new decision logic: `Evaluate` is still the only
judge, and the engine still only decides how many samples to hand it.

## Alternatives considered

- Lower the minimum sample count instead: would reduce the false-inconclusive rate everywhere, not
  just for promising-but-unresolved readings, at the cost of also reducing power against real
  regressions and inflating the first series's runtime for every candidate, not only the ones that
  need it.
- Confirm every inconclusive verdict, not only ones resting on an unconfirmed improvement: wastes a
  second series on a candidate that is inconclusive for an unrelated reason (missing evidence, a
  guardrail that cannot be evaluated), where more samples of the same metric would not change the
  outcome.
- Give the improvement side its own pair count independent of `confirmRegressions`: rejected for
  the same reason ADR 0016 folds workloads together rather than per-seed — the pooled reading needs
  series of one length across every seed, and a second, differently-sized series would not compose
  with a regression confirmation that already ran.
