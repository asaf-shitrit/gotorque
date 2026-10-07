# 0038. The engine bridge is one port, `Bench`

- Status: accepted
- Date: 2026-10-07
- Relates to: [0035](0035-the-optimizer-is-the-only-model-role.md) (the real seams that remain), [0036](0036-candidate-evaluation-is-its-own-module.md) (what sits behind `Assess`), [0037](0037-discovery-is-its-own-module.md) (what sits behind `Discovery`); [0009](0009-deferred-evaluation-module-seam.md) is superseded by 0036 and not reopened

## Context

The orchestrator reached the engine through four interfaces in `internal/orchestrator/services.go`:
`RunnerService` (`Inspect`, `Discover`, `EvaluateCandidate`, `PromoteCandidate`), `ExcerptCollector`
(`CollectExcerpts`, reached by a type assertion so that fakes that did not implement it compiled),
`PolicyService` (`Evaluate`) and `JobService` (`StartCampaign`, `RecordProgress`, `CompleteCampaign`,
`RecordRoleDegraded`, `RecordRoleRepaired`). Each had exactly one production adapter, `adkServices`, and
a family of test fakes (a runner, an accepting runner, an unmeasured runner, a hot runner, a proposal
recorder, three policies, a job service). They were hypothetical seams: one adapter each, with a second
implementation that only existed to satisfy the interface in tests. Reading the adapter showed what the
shape had accreted:

- `Inspect` copied the engine's inventory into `CampaignState.Inspection`, which nothing read.
- `Discover` returned state that never changes inside a campaign, and the graph asked again every cycle.
- `PolicyService.Evaluate` is documented as deterministic and was the place the verdict was recorded:
  it ignored `PolicyInput.Campaign`, ran `policyVerdict`, appended the candidate record, saved the event
  and snapshotted the report. Promotion (`PromoteCandidate`) and the tallies (`RecordProgress`) were two
  more calls the graph made afterwards, so a failure between them could leave a record whose accepted
  patch never landed.
- `CompleteCampaign` re-derived "did the campaign fail" from `ProviderFailure`, which the termination
  module (`ended`) had already decided. `domain.Job` existed to be passed around that.
- The policy never reads the review (`policyVerdict` and `internal/policy` have no use for it), yet the
  verdict was computed after the reviewer node, which let "agents advise, code decides" be a convention
  of ordering rather than a property of the graph.

## Decision

The graph reaches the deterministic half through one port:

```go
type Bench interface {
    Discovery(ctx) (DiscoveryEvidence, error)                          // once per graph entry
    Excerpts(ctx, analysis agents.AnalystResult) ([]SourceExcerpt, error)
    Assess(ctx, CandidateRequest) (Assessment, error)                  // evidence and verdict
    Settle(ctx, Settlement) error                                      // record, promote, tallies
    Note(ctx, Note) error                                              // started, degraded, repaired, finished
}
```

`Assessment{Evidence, Verdict}`: `Assess` builds, tests and measures the proposal and judges the
evidence with the campaign's policy, so the verdict exists before the reviewer runs. The graph checks
that the verdict is one of the three decisions and names the evidence's candidate. `Settlement` carries
the assessment (the evidence with the optimizer's output repair stamped on it), the target, the review
and the tallies after the verdict. `Settle` records the verdict it is handed; it does not recompute it.
Order inside `Settle`: an accepted patch is copied to `accepted/` first, the record is saved with its
accepted marker in one state (`candidate_evaluated`), then `candidate_accepted`, and the tallies last
(`adk_progress`). A record on disk therefore always has its promotion, and a bound never counts a
verdict that is not recorded. A patch that cannot be copied fails the settlement before anything is
saved, so a resumed campaign evaluates the attempt again. Recording first and promoting second would
leave a record without its promotion after a failure between the two, which is what the old three calls
could do; promoting first cannot, and an orphaned copy with no record is harmless. `Note` carries four kinds of one record; the graph ignores a failed degraded or repaired
note and fails the run on a failed started or finished one, as it did.

The inspection node, `Inspection`, `DiscoveryRequest`, `PolicyInput`, `domain.Job` and its status
constants, and the job fields of `CampaignState` and `CampaignResult` are deleted. `run_discovery` stays
as a node, once per graph entry, between `initialize_campaign` and the first analyst; cycles route back
to the analyst. Node names stay (`finalize_campaign` is matched by `collectADKResult`, and the degrading
wrappers report under role names), `apply_policy` keeps its name though it now only counts and settles,
and every event kind and order the report, scorecard and triage read is unchanged. The one change to
recorded data is that `adk_finalized` and `adk_completed` payloads no longer carry a `job` object.

The seams that stay are the ones where behavior varies: the optimizer (`agents.Set`, a live model or a
stub), and the two Jev advisors (`CauseAnalyst`, `ReviewAnalyst`, live or `jev.Stub`). They are
degradable advisors and are not folded into the bench.

## Consequences

The engine is the only production `Bench` (`engineBench` in `internal/campaign/adk.go`); the graph tests
use one `fakeBench` whose variants are fields, or an embedding type that overrides `Assess`. The orchestrator
suite went from ten fakes to one (plus one override type in a single test), and the excerpt path, which no fake used to reach, is now exercised.
Moving the verdict ahead of the reviewer changes no answer today and makes it structural: a test pins
that a review that objects to everything cannot turn an accepted candidate away.

Cost: a wider interface than any of its predecessors, five methods. It is wide because it is the whole
deterministic half, and the invariants that matter (the verdict precedes the reviewer, settlement does not
recompute it) are properties of how its calls relate, which is where a seam should be. It does not hide
anything an agent could reach: it exposes no general shell API, only what the graph already called.

A resumed campaign still enters the graph afresh and fetches discovery once; its ledger and tallies come
from `RecordedCandidates` as before.

## Alternatives considered

Keep four interfaces and delete the dead ones (rejected: leaves `Discover` and the settlement calls as
separate ports each with one adapter, and keeps the optional `ExcerptCollector` assertion that hid the
excerpt path from every fake). Split `Bench` into a read half and a write half (rejected: the graph
needs both from the same object in the same order, and a split would reintroduce the ordering as
convention). Fold the Jev advisors into `Bench` (rejected: they are advisors that degrade, and a failing
advisor must not look like a failing bench).
