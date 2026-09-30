# 0034. Borderline accepts are measured again before the verdict

- Status: accepted
- Date: 2026-09-30

## Context

ADR 0016 measures an insignificant over-limit regression again before judging it, and ADR 0021 does
the same for an insignificant improvement past the minimum. A *supported* improvement past the
minimum was accepted on its first series of 25 pairs.

The first held-out sweep accepted dyff on a supported -3.16% on one workload, against the 3%
minimum, with the pooled reading at -1.23%. `gotorque verify` measured it again over 60 fresh pairs.
The win was real, but -2.17% on that workload and -1.18% pooled, so the verdict was inconclusive.
A reading chosen because it is the best of several, and that clears the bar by a hair, is where
chance inflates an estimate across the threshold. By contrast, tomlv's -41.68% verified at the same
size.

## Decision

When the verdict would be an accept and the reading it rests on (the best supported improvement)
cleared the minimum by less than a factor of two, every workload is measured over a second series
before the verdict (`policy.BorderlineImprovements`, consumed by `confirmImprovements`). The policy
then decides on both series together, unchanged. The one-extra-series-per-candidate bound still
holds.

## Consequences

- Near-threshold accepts cost one more series: seconds for a short CLI, bounded by the campaign
  deadline.
- A real win near the threshold still accepts when both series together support it past the
  minimum. An overestimate drifts back under it and ends inconclusive, as dyff's would have.
- The factor of two is a judgment. It covers the region where one series' spread can move a
  reading across the threshold, and leaves clear wins, which verified at their original size,
  alone.
