# 0004. Measurement uses 25 interleaved pairs and a benchstat lane verified against the real binary

- Status: accepted
- Date: 2026-09-20

## Context

Two independent defects made real effects read as noise. Seven pairs could not resolve a 3%
effect against the 6-8% per-run spread of a 10-25 ms workload: a gron candidate the campaign
recorded as inconclusive at -3.51% measured -3.75% with p<1e-4 over thirty pairs on the same
binaries. And the sample files written for benchstat were one bare value per line, which is not a
benchmark report: benchstat skipped every line, exited 0 and printed nothing, so the lane had never
contributed a p-value and a pooled -11.69% turned out to be two outlier samples (19.7 ms and
16.7 ms against a 9.9 ms median) that benchstat scores as `~ (p=0.648 n=7)`.

## Decision

Twenty-five interleaved pairs per workload; sample files in the Go benchmark format benchstat
parses; and a test that drives the installed benchstat binary over freshly written samples and fails
if it reads nothing.

## Consequences

About a second of extra measurement per workload. Supported/unsupported verdicts now rest on real
p-values (for example `-6.70% (p=0.000 n=25)`), and a missing benchstat still degrades to the
internal t-test.

## Alternatives considered

Keep seven pairs (rejected: underpowered for the manifest's 3% bar). Trust the canned-executor
tests alone (rejected: they cannot see a format mismatch, which is how the lane stayed dead).
