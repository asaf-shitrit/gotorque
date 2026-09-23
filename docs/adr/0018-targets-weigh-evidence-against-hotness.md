# 0018. Targets are ordered by evidence discounted by hotness

- Status: accepted
- Date: 2026-09-23

## Context

ADR 0013 has code choose each candidate's target and walk the targets in hotness order: every
site's first flagged cause, hottest function first, before any second cause. It rejected ordering
by Jev's z-score, because on gron the accepted patch was the top target by hotness.

On gron that target (the output loop's unbuffered I/O, +3.47 sd) was also the strongest by a wide
margin among the hottest functions, so gron did not distinguish the two orders. gojq did. Once its
manifest measured a realistic workload, its hottest functions were the encoder's `flush`, `marshal`
and the input iterator, and the campaign spent all three candidates on fast-path flags at +1.39 to
+1.69 sd. None of them measured a difference. `printValues`, fifth by hotness with unbuffered
output at +3.41 sd, was never reached. A bufio fix of it, applied by hand, measured -12.8% on the
same workload with identical output and no new test failures.

Pure z-order is no better: on gron it would try `statementsFromJSON`'s fast path (+3.97 sd, fifth
by hotness) before the output loop, and gron's fast-path micro-optimizations have measured
inconclusive before.

## Decision

Within each tier (every site's first cause, then every site's second), targets are ordered by
z / sqrt(1 + rank), where rank is the function's position in discovery's hot list. Ties keep
hotness order. Strong evidence a few places down outranks weak evidence at the top; far down
the list only much stronger evidence does.

## Evidence

Replayed over the first causes of all five recorded Jev analyses:

| Campaign | Hotness order picks first | z-order picks first | This order picks first |
| --- | --- | --- | --- |
| gron #5, gron confirmation | `gron` unbuffered I/O (accepted) | `statementsFromJSON` fast path | `gron` unbuffered I/O |
| gojq #1, #2 | `printValues` unbuffered I/O | `printValues` | `printValues` |
| gojq, realistic workload | `marshal` fast path (inconclusive) | `printValues` | `printValues` |

## Consequences

The order is a heuristic fitted to five analyses from two targets, and the square root is a
choice, not a measurement. It keeps ADR 0013's rule that code, not a model, picks the target, and
the verdict still rests on measurement. A benchmark over more targets could replace it with a
measured weighting.
