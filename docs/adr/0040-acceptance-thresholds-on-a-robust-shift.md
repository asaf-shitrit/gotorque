# 0040. Acceptance thresholds on a Hodges–Lehmann shift, regressions keep the mean

- Status: accepted
- Date: 2026-10-24

## Context

A verdict's numbers all came from the sample mean. On an interleaved A/B series a few slow runs on
one side move that mean far from the typical effect, and the acceptance side is where that matters:
a win inflated across the minimum becomes an accept.

The recorded campaigns show it. Replaying 306 measured candidates from 93 development-target
campaigns, the best supported improvement in go-jsonnet's `order-report` read -5.16% by the mean. Its
25-pair baseline held two runs at 85.4 ms and 86.5 ms against fourteen under 64 ms; the
Hodges–Lehmann shift of the same samples is -2.56%, under the 3% minimum. The mean accepted it.

## Decision

Every sample-derived `domain.MetricComparison` records, beside `DeltaPercent` (the sample mean), a
`ShiftPercent` and the estimator that produced it (`Shift: "hodges_lehmann"`). The shift is the
median of every candidate-minus-baseline pairwise difference, as a percentage of the baseline
median (`campaign.hodgesLehmannShift`, `robustShiftPercent`).

Acceptance reads the shift: the best eligible improvement, the minimum threshold, the accept
reason, and both confirmation triggers (`UnconfirmedImprovements`, `BorderlineImprovements`, ADR
0021, ADR 0034) use `policy.ImprovementDelta`, which returns the shift when one was measured and
the mean otherwise. A comparison without a shift (binary size, too few samples) falls back to the
mean.

Support is unchanged: an improvement still needs significance (ADR 0039), and the shift only
decides which magnitude the minimum is compared against. A supported mean win whose robust shift
sits under the minimum is inconclusive, not accepted.

Regression and guardrail checks keep `DeltaPercent`. The report shows both columns, and the event
summary shows both numbers when they differ.

## Evidence

`TestRecordedVerdictsSurviveTheRobustShift` replays the campaigns recorded on this machine (skipped
when absent, as in CI) and never reads a campaign on a held-out target. It recorded:

- 306 measured candidates from 93 campaigns.
- One verdict changed: go-jsonnet's `order-report` accept became inconclusive, because the shift
  (-2.56%) is under the minimum. That accept rested on the two slow baseline runs; the shift also
  puts it inside ADR 0034's borderline band, so a live campaign would measure it over a second
  series before the verdict.
- No candidate the mean accepted was rejected, and none it refused was accepted.

The alternative — switching the regression side too — was replayed and declined: it changed 17
verdicts, six of them A/A null candidates (null2-fzf 12, null2-scc 10, 11) going from inconclusive
to rejected on a reading whose mean stayed under the 2% limit. A robust estimate can understate a
real regression as easily as it can understate a real win, and a false rejection is not a safe
failure. The mean with the significance gate (ADR 0016, ADR 0033) stays the regression instrument.

Unpaired rather than paired differences: the verdict's t-test already treats the two samples as
independent, and the paired estimator produced one new accept in the replay (`live7-go-jsonnet` 2)
that the unpaired one did not.

## Consequences

- A win carried by slow baseline runs no longer accepts; the reported improvement is the typical
  shift, with the mean beside it.
- A win the mean understates is still accepted when the shift clears the minimum, which the small
  replay counts show happens less often than the reverse.
- A comparison with fewer than two samples per side, or a non-positive baseline median, keeps the
  mean delta and accepts on it, as before.
- The regression ceiling is still a mean threshold. A guardrail regression that only the mean shows
  remains a rejection; closing that would need raw guardrail samples persisted, which the recorded
  campaigns do not carry.
