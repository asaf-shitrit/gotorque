# 0048. Interleaved pairs are judged by a paired test

- Status: accepted
- Date: 2026-11-03

## Context

`RunInterleaved` runs baseline and candidate in pairs, alternating which side goes first (ABBA), so
both halves of a pair see the same machine and the position effect is spread over both sides (PR
#31: on miller, two byte-identical binaries measured the second run of a pair 2.66% slower in 41 of
60 pairs). It returns the results paired by index. The decision then threw the pairing away: `compareMetric`
judged the two columns as independent samples with a Welch-style `|t| > 2.2`, and for wall time
benchstat's Mann-Whitney U p-value replaced it whenever the binary was installed.

An unpaired test counts every difference between pairs as noise. On a machine whose speed wanders
between pairs, the quantity that cancels inside a pair is exactly what the test measures the effect
against. The tests stay conservative (false positives near 0%) and lose power instead, which reads
as safe until the power is measured. Asaf decided on 2026-11-03 (asaf-shitrit/blog#10, issue #131)
to switch the decision first and write the post about the before and after.

## Decision

Significance is a two-sided Wilcoxon signed-rank test on the per-pair differences, candidate minus
baseline, at `p < 0.05` (`internal/campaign/paired.go`). The p-value is exact for up to 100
untied pairs and a tie-corrected normal approximation with continuity correction otherwise. Pairs
with a zero difference are dropped, and fewer than four pairs never count.

`compareMetric` uses it for every metric whenever the two series have equal length (the shape
`RunInterleaved` produces; a pair with an unusable value on either side is dropped whole). Pooled
readings, which fold the seeds per repetition, are still paired by repetition. Two series that cannot
be paired keep the previous Welch path.

`StatisticallyFit` keeps its two routes (ADR 0039): significance, or, for a reading that is not an
improvement, a 95% interval below the metric's limit. The interval for a paired reading uses the
standard error of the per-pair differences, with the same critical value of 2.2. An improvement still
needs significance.

Each comparison records `test` (`wilcoxon_signed_rank` or `welch`) and, for the signed-rank test,
`p_value`, so a verdict says which test it came from. Benchstat still runs and its output is kept in
the report, but for paired series it does not decide: its rank test compares the two columns as
independent samples, which is the assumption this ADR removes. It still decides for series that cannot
be paired, as before (ADR 0004).

Nothing else changes. The minimum improvement, the regression and guardrail ceilings, the
Hodges-Lehmann magnitude (ADR 0043), estimator agreement, the borderline and confirmation series
(ADRs 0021, 0034, 0040, 0041), the stress-seed veto (ADR 0047) and the policy order all read the same
fields as before.

## Evidence

`TestPowerOfPairedAndUnpairedTests` (`internal/campaign/power_test.go`) simulates 25 interleaved
pairs the way `RunInterleaved` orders them: log-normal noise, a shared AR(1) drift (rho 0.9), and an
optional 2.66% second-position effect. It counts how often each test calls a series significant, for
a -3% candidate (power) and for identical binaries (false positives). 4,000 trials per cell,
`GOTORQUE_POWER_TRIALS=4000 go test ./internal/campaign -run TestPowerOfPairedAndUnpairedTests -v`:

| Noise model | Effect | Mann-Whitney U (before, with benchstat) | Welch `\|t\|>2.2` (before, without) | Signed-rank (after) |
|---|---|---|---|---|
| iid 7% noise | -3% | 29.2% | 25.1% | 29.1% |
| | none | 4.7% | 2.9% | 4.7% |
| 6% drift, 2% within-pair noise | -3% | 65.2% | 57.5% | 99.9% |
| | none | 0.1% | 0.1% | 4.7% |
| 6% drift, 2% noise, 2.66% order effect | -3% | 54.9% | 47.5% | 96.7% |
| | none | 0.0% | 0.0% | 0.9% |

With no drift the tests are equal. With drift the unpaired tests detect a 3% win in 48% to 65% of
series and the paired test in 97% to 100%, with the false-positive rate at or under the nominal 5%. The
default 400-trial run asserts the same ordering with margins. The model's parameters are the ones
in the blog#10 backlog entry; which cell matches a real machine is not established, because no
recorded campaign directory exists on the machine this ADR was written on, so a replay of past
campaigns (as ADR 0043 did) was not possible. `verify` and `--null-candidates` on a live target
are the check that remains.

## Consequences

- A real effect that drift hid from the unpaired tests now reaches `significant`. More candidates
  will accept, and more will reach the borderline re-measurement. Whether any accepted patch is
  not real is what `verify` and the null-candidate false-accept rate measure; this change has not
  been measured on a live target.
- The false-positive rate of a single reading under drift rises from near 0% (the unpaired tests were
  conservative) to the nominal 5%, as in the no-drift row. An accept still needs a Hodges-Lehmann
  shift past the 3% minimum on a reading that is significant, then the borderline re-measurement
  (ADR 0034) and the selected-workload re-measurement (ADR 0041). Whether the false-accept rate on
  null candidates stays at zero is the thing to watch when the held-out sweep is next run (#103).
- The README's claim that benchstat's rank test supports a verdict is replaced by the paired test.
  A machine without benchstat now judges paired series exactly as one with it.
- The signed-rank test assumes the differences are independent and symmetric about zero under no
  effect. ABBA alternation makes the position effect symmetric to within one pair (13 baseline-first
  against 12 candidate-first at 25 pairs); the simulation's order-effect null shows no inflation. A
  confirmation series restarts the alternation, so a pooled series is balanced to within two pairs.
- The magnitude estimator is unchanged. ADR 0043 chose the unpaired Hodges-Lehmann shift because the
  test was unpaired, and recorded that the paired estimator produced one more accept in its replay
  (`live7-go-jsonnet` 2). That reasoning no longer matches the test, and revisiting it needs a
  replay on recorded campaigns, so it stays a separate decision.
- Comparisons recorded before this change carry no `test`; they were judged unpaired.

## Alternatives considered

- **Paired t-test.** It matches the signed-rank test under drift (99.9% in the simulation), but one
  slow pair moves it as the mean moves, which ADR 0043 already declined for acceptance. The rank
  test keeps the robustness benchstat's rank test was chosen for (ADR 0004).
- **Sign test.** Less power (98% under drift in the backlog's run) for no gain in assumptions.
- **AB/BA crossover analysis**, estimating the position effect and removing it. It needs the order of
  each pair recorded with the sample, and today the order is implied by index parity within a series.
  Alternation already balances the effect, and the simulation shows the signed-rank test unaffected by
  it, so the extra bookkeeping buys nothing measurable yet.
- **Keep benchstat as the decider and add the paired test beside it.** Two tests that disagree need a
  rule for the disagreement, and the unpaired one is the one that is wrong under drift.
