# 0041. An accept that picks the best workload reading is measured again

- Status: accepted
- Date: 2026-10-24

## Context

A candidate may accept on any acceptance-eligible reading, which includes one
reading per representative workload. With several workloads the campaign picks
the one that read best after 25 pairs, and the best of k noisy estimates is
biased upward: the winner's curse. ADR 0034 re-measures an accept whose best
reading cleared the minimum by less than a factor of two, and it pooled the new
pairs with the ones that selected the reading, which keeps part of the bias.
Readings that cleared the bar by more were accepted on their first series.

Two recorded accepts failed that way. `daily-2026-10-02-devB-miller` attempt 4
accepted `(*Reader).readRecord` at -6.51% on `filter-value-threshold`, the best
of five workloads, against a 3% minimum; `gotorque verify` over 60 fresh pairs
put the same workload at -3.04% and the verdict was inconclusive. In
`daily-2026-10-03-heldout-s2c` attempt 3 a semantically no-op patch was
accepted at -4.24% on one of two `encodeBlockBest` workloads while the other
moved +2.55%. `gotorque verify` was inconclusive twice.

The selection bias lives in the reading, not in the workload: a fresh series on
the same workload is an unbiased estimate of that workload's effect. Pooling it
with the series that chose the workload keeps the inflation, because the
selecting pairs are still what made it the best.

## Decision

When the verdict would accept on a per-workload reading (`policy.SelectedImprovements`),
that workload alone is measured over a fresh series, and every comparison is
derived again from what is now measured (`confirmSelection`). The series that
selected the workload is replaced, not extended: the verdict rests on the fresh
series alone.

The rule repeats while the verdict still rests on a per-workload reading, so a
candidate whose fresh reading no longer clears the bar but whose second
workload's does is measured again there rather than accepted on the reading
that selected it. Each workload is measured at most once more, and the campaign
deadline bounds the cost as it bounds every series. A target with one
representative workload offered no choice among readings and is not re-measured.

The re-measurement is the candidate's one extra series. A candidate whose
regression or unsupported-improvement confirmation already ran is not measured
again for selection, and a fresh reading that ends unsupported stays
inconclusive rather than earning a third series.

## Consequences

- A per-workload accept costs one more series of the same 25 pairs, seconds for
  a short CLI. A clear single-workload win is now re-measured too, where ADR
  0034 left it alone.
- ADR 0034 still governs a pooled accept near the threshold; for a per-workload
  accept this ADR replaces its pooled second series, which was the case that
  motivated it (dyff was a single-workload accept).
- The record holds the series the verdict rests on. The selecting series is not
  in `RepSamples`; the summary names what it read beside what the fresh series
  read, so the discarded reading stays auditable in words.
- An accept no longer survives on a reading chosen by the series that selected
  it. The false-accept rate a selected reading contributes falls toward that of
  a single measured reading, at the cost of some real wins near the threshold
  ending inconclusive.
