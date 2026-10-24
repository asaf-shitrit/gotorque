# 0041. An answer refused before it could be judged is asked again once, in the same attempt

- Status: accepted
- Date: 2026-10-24

## Context

With a code-chosen target the optimizer is told to return `function_source`
instead of hand-writing a diff (ADR 0022), because diff mechanics were the most
common way a candidate was lost before measurement. The instruction is not
binding, and the harness deliberately keeps a hand-written `patch` working as a
fallback. When a model ignores it, the fallback costs the whole attempt: the
patch fails `git apply --check`, `Prepare` reports the error, and the candidate
is recorded as `Unmeasured` and rejected. Nothing about the *change* was judged,
only how it was written, and the transport that answers that failure exactly —
deterministic diffing of a declaration the target already located — was never
asked for.

The record shows how often. Across the 151 campaigns under
`~/projects/gotorque-work/campaigns` (522 candidate records), five candidates
were rejected before build with an apply refusal. Four of them had a
code-chosen target and a hand-written patch with no `function_source` beside
it: `live3-cue` attempts 2 and 3, `heldout4-s2c` attempt 3, `heldout5-otto`
attempt 1. `overnight-seqkit-1` attempt 4 had no target, so no transport was
owed and nothing would have been asked differently.

Sibling issue #77 describes the same shape of loss twice over: a
`function_source` that stops partway through, and an answer that carries no
code at all. Its triage decision requires one shared trigger with this issue's
rather than separate mechanisms per failure.

## Decision

`internal/orchestrator`'s `evaluate_candidate` node may route back to the
`optimizer` node once per attempt instead of settling the verdict it was
handed. The trigger, `reaskable`, is deterministic and narrow: the cycle has a
code-chosen target, its one re-ask is unspent, the evidence carries a
`ReaskCause`, and the proposal hand-wrote a patch with no `function_source` to
fall back on. `ReaskCause` is set by the engine's `prepareCandidate` when, and
only when, `candidate.WorktreeManager.Prepare` fails with
`candidate.ErrPatchDoesNotApply` — a new sentinel wrapping the `git apply
--check` failure whose fuzzy fallback also found no home.

The re-ask is a second call to the same optimizer node with the same brief plus
the refusal: `CampaignState.ReaskCause` travels into `OptimizerBrief`, the
instruction says what a turn carrying it wants (a complete `function_source`
and `imports`, patch left empty), and the node path records a `role_reasked`
event naming the refusal. The second answer is judged by every gate the first
would have faced. A `NoteReasked` reaches the journal through `Bench.Note`, so
a campaign with two optimizer calls and one settlement says why.

`ReaskCause` is deliberately not an apply-specific field. #77 widens the same
trigger — a `function_source` that does not parse at EOF, an answer with no
code at all — by setting it from its own pre-build rejection and adding its
condition to `reaskable`. One ask per attempt stays the rule.

## Consequences

- An attempt may spend two optimizer calls. The re-ask is inside the attempt,
  so `Attempt`, `CandidatesTried` and the `max_candidate_patches` budget are
  unchanged; the rejected first answer is written to `patches/` as any
  proposal is, but earns no candidate record and no verdict.
- A `patch` that arrives beside a `function_source` keeps the precedence ADR
  0022 gives it, and is not re-asked: the transport was available and lost for
  another reason.
- A campaign with no code-chosen target never re-asks. There is nothing
  deterministic to ask for, so the same question twice would only cost a call.
- The second call is inside `evaluate_candidate`'s sibling node, not inside the
  evaluation, so the per-node `DeterministicTimeout` still bounds one
  evaluation and the agent timeout bounds the model call.
- The record cannot show how many of the four targeted attempts would have
  ended differently. The re-ask removes the transport failure and nothing more:
  the `function_source` may still not parse, build, or pass the gates, and a
  live measurement on development targets is what would say whether it pays for
  the extra call.

## Alternatives considered

- **Fall back to `function_source` when the patch fails to apply.** There is
  none: the proposal that reached this path carried only a patch, which is the
  trigger's defining condition.
- **Re-ask from inside `Bench.Assess`.** Evaluation takes no model role (ADR
  0035) and is bounded by `DeterministicTimeout`; a model call there would move
  the time bound's meaning and give the deterministic half a role to call.
- **Retry the hand-written patch with the error quoted.** The model already had
  the target's source and produced a diff it could not align; the failures in
  the record are misplaced hunks and a corrupt diff. Asking for the same wire
  format again, rather than for the declaration the deterministic path wants,
  repeats the failure that cost the attempt.
- **Loop until the patch applies.** One ask bounds the cost per attempt and
  keeps a model that cannot use either transport from spending the campaign.
- **Make the re-ask unconditional for any unmeasured rejection.** A
  protected-path or shape rejection is about the change itself, and the answer
  to it is a different remedy, not a different transport; asking again would
  spend a call to be told the same thing.
