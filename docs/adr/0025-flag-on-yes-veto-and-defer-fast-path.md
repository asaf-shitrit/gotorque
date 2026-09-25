# 0025. A cause is flagged only when Jev says yes; code vetoes what it can rule out; fast path goes last

- Status: accepted (with `--analyst jev`)
- Date: 2026-09-25

## Context

Jev's cause answers become targets in two steps: each answer is z-scored against Jev's usual answer
to that question, and causes at +0.5 sd or more are flagged, at most two per function (ADR 0012).
Code orders the flags into targets (ADR 0013, 0018).

The live record showed the z gate choosing badly. Over 38 targets attempted in Jev campaigns, 5 led
to accepted fixes. Every one of those had Jev's own probability at 0.67 or more, and none of the 14
targets whose probability was below 0.5 was accepted. The highest z-scores in the record were all
fast_path: 17 fast_path targets were tried, with z up to +4.6, and none was accepted, including 8
Jev answered yes to. On dasel, 9 of 11 candidates went to fast_path targets and all came back
inconclusive. An audit showed dasel's real win was an allocation that the function's callers
discard, and that the fast-path patches added branches that never fire.

## Decision

- **Probability floor.** A cause is eligible only when its z is at least 0.5 *and* Jev's
  probability is at least 0.5. An ineligible cause is skipped, not a stop, so a lower cause that
  clears both gates still gets one of the two slots.
- **Code vetoes** for the two causes whose mechanism is visible in the source:
  - `unbuffered_io` needs a read or write that reaches a real file or stream. This generalises the
    in-memory override of ADR 0019, which it replaces.
  - `prealloc` needs a container that grows inside a loop.

  A function the parser cannot find is never vetoed. Vetoed and skipped causes are named in the
  `cause_analysis` event.
- **fast_path is deferred, not dropped.** It passes the same gates, but its targets form a final
  tier after every other cause.
- Target order is otherwise unchanged.

## Evidence

The rule was chosen on the 93-fix benchmark and evaluated once on 60 held-out fixes, labelled before
any question was asked:

| | Current rule | This rule |
|---|---|---|
| Flags on already-fixed code (held-out) | 0.30 | 0.20 |
| Any action on a fixed function (held-out) | 0.78 | 0.47 |
| Recall outside fast_path (held-out) | 0.44 | 0.47 |

Precision of the flagged cause rose on the benchmark (+0.15) but not significantly on held-out
(+0.03): the rule makes gotorque act less often on weak or futile flags, not name causes better.

Replayed on the recorded live analyses, every accepted fix moves to first or second place (yq 4th to
2nd, go-jsonnet 4th to 2nd), gron's target list shrinks from 17 to 7, and dasel's from 20 to 9 with
no fast_path target ahead of the others. The fast_path deferral rests on the same live record it is
replayed against, so only a campaign on a new target is an independent test of it.

A variant that derives alloc, fast_path and redundant from pairs of fix-kind questions ranked
causes better on held-out (top-1 0.43 against 0.32), but with this rule it flagged them no better
(precision 0.33 against 0.37, recall 0.33 against 0.42). It is kept on the unmerged `jev-f1x`
branch with its measured baseline.

## Consequences

Fewer targets per campaign, and some functions get none: the held-out abstention rate rose from 13%
to 32%. The code vetoes know only the call shapes they list, and doubt leaves Jev's flag standing.
Waste that is visible only from a function's callers, as on dasel, is still out of reach: neither
the questions nor a caller-context field helped on the benchmarks, whose fixes are all inside one
function, and the shape check still confines a patch to the target function.
