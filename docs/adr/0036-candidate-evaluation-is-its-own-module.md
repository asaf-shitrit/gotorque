# 0036. Candidate evaluation is its own module

- Status: accepted
- Date: 2026-10-07
- Supersedes: [0009](0009-deferred-evaluation-module-seam.md)

## Context

ADR 0009 deferred giving candidate evaluation its own module until a concrete need appeared: a
second evaluator, or tests that needed fakes rather than a real campaign directory. Both have
appeared. Evaluation now has three callers with different settings: the campaign's own attempts,
`gotorque verify` (60 pairs, duplicate and accepted-fix refusals off) and `--null-candidates`. The
interface they shared was a fully populated `Engine` plus mode flags set by mutation, and verify
expressed its difference by setting `Engine.verifying` and `Engine.repetitions` around the call. That
leaked:

- The confirmation notes hard-coded the campaign's 25 pairs, so a 60-pair verification recorded
  "after 25 pairs ... 25 more" over a record of 120 samples per workload.
- The informational PGO lane ran during verification, at 60 pairs, into a record that has no PGO
  columns.
- The test gate's `pruneUnstablePasses` rewrote the campaign's persisted required-test set while
  judging a verification, so verifying one accepted candidate permanently narrowed what every later
  candidate was held to and spent one of the two baseline re-checks.

Other symptoms had the same cause. Null candidates re-implemented the verdict recording the graph's
decision node does (append the record, save the event, snapshot the reports). The contended
re-measurement that guards against false accepts (a first pass that ends on a loaded machine is
discarded and measured again, after overnight-miller-1's false accept) had no test, because the load
average was a process global and every test disables isolation. And the `function_source`,
`function_sources`, untargeted and null-patch transports read their source from the canonical
checkout, which anything may dirty (miller's tests rewrote tracked fixtures in it), while the test
gate named the base revision from the environment and evaluation named it from the request.

## Decision

Evaluation is an unexported `evaluator` in `internal/campaign` that holds no `*Engine`.
`Engine.newEvaluator` builds one per evaluation from the campaign's current state (the records, the
baseline's passes and the discovery profile change between calls), and `evaluate(ctx, req, settings)`
returns the `orchestrator.CandidateEvidence` the graph already consumes. `Engine.evaluateCandidate`
is a thin wrapper that passes the campaign's settings.

What differs between callers is an argument, `evalSettings{pairs, refuseRepeats, pgoLane, baseline,
known}`, chosen by each caller: the campaign's (`campaignSettings`), verification's (its pair count,
no refusals, no PGO lane, a scratch copy of the test baseline) and the null loop's (the campaign's).
The confirmation notes take their pair count from it.

The evaluator reaches outside itself through three ports, each with a real second adapter:

- `journal`: the events an evaluation reports and the sandbox isolation notes its runs observe. The
  campaign's adapter saves synchronously through `saveEvent`; tests substitute a recording one.
- `machine`: load sampling, the contended threshold and the bounded quiet wait. The host's adapter is
  the old behaviour; tests script a load burst and cover the discarded-and-measured-once pass.
- `testBaseline`: the tests the gate holds a candidate to, which the gate may narrow. The campaign's
  adapter writes through to the persisted state; verification passes a copy that persists nothing.

`toolchain` and `runner` stay concrete: a real Git and a real runner are what tests use as well.

Source transports read from a base tree the evaluator owns: a pristine worktree of the base revision
under the campaign directory, created on first use, checked at the base revision with no change (and
created again when it is not), and removed when the evaluation ends. One base revision is used
throughout. Verdict recording is one function, `recordVerdict`, used by the graph's decision node
and the null loop.

`shape.go`'s interface and `candidate.WorktreeManager` are unchanged, and so are the ordering
invariants and protected-path checks of ADR 0017 and the test gate.

## Consequences

The deletion test passes: delete the evaluator and its complexity reappears in each caller, which
is what happened before it existed. Evaluation can be driven stage by stage in tests with no
campaign (`measuredEvaluator`), and the contended re-measurement, the journal and the baseline
narrowing each have a test. A fourth caller picks its settings instead of adding a flag to the
engine.

Constructing an evaluator copies a handful of fields per evaluation, which is noise next to a build
and a measurement. The base tree costs one worktree creation for each `function_source` candidate,
and none for a plain patch.

Not done here: the PGO lane's baseline build still builds the canonical checkout (a build, not a
source read); the `machine` port covers load, not the runner's own isolation.

## Alternatives considered

Keep the flags on `Engine` and add the missing pair count to the notes (rejected: it fixes one
leak and leaves the mechanism that produced three). Make the evaluator hold the settings as state
(rejected: a per-call value cannot leak into the next call). A `store` port instead of `journal`
(rejected: evaluation reports events, not state, and nothing else in it needs the store).
