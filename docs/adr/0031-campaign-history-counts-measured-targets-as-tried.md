# 0031. Earlier campaigns' measured targets count as tried (`--history`)

- Status: accepted
- Date: 2026-09-27

## Context

Target choice is deterministic (ADR 0013): `planTarget` takes the first flagged target no earlier
candidate of this campaign tried. Nothing carried that knowledge across campaigns, so every new
campaign on a repository started from the same ranking and spent its first attempts on the same
targets. On dasel, the `WithExecutorID` string-building rewrite came back inconclusive (about 0%)
in three consecutive bounded campaigns, and `newPtr` and `exprExecutor` followed the same path.
With a 4-candidate bound, each repeat cost a quarter of the campaign.

## Decision

`gotorque optimize --history DIR` (repeatable) names earlier campaign directories. When a campaign
is created, `loadHistory` reads each one through `LoadReport` and collects the target of every
candidate that reached measurement (accepted, or compared against the baseline at all). Those
targets are persisted as `State.HistoryTargets` and prepended to `priorTargets`, the same list a
resumed campaign uses to skip targets it already tried.

- **Same revision only.** A past campaign at a different base revision is recorded in
  `HistorySources` as skipped, with both revisions. A target's location may point at different
  code once the source moves, so it is not trusted.
- **Measured only.** A candidate rejected before measurement (failed to apply, the shape check, or
  the build) says nothing about its target, so its target is not carried. This matches the
  in-campaign rule that an unmeasured target gets a retry.
- **Persisted at creation.** The flag is read once. A resume uses the stored list, and `--resume`
  rejects `--history`.
- **Visible.** The report gets a "Campaign history" section (sources, targets carried, skips), and
  its reproduction line repeats the `--history` flags.

This is bookkeeping on the deterministic side. No agent sees the history or decides from it; it
only changes which target code picks next.

## Consequences

A series of bounded campaigns on one revision now walks down the ranking instead of re-measuring
its head. An accepted target is carried too: its fix is not in the base revision, but it has
already been found, and re-finding it costs an attempt. A caller who wants a fresh ranking
leaves `--history` off.

The history is not a verdict store. A carried target is only "tried". It can still be the subject
of a later campaign at a new revision, where its source may have changed.

## Alternatives considered

- **An implicit per-repository ledger** (e.g. under the user cache directory): it would change
  target choice based on files the invocation does not name, which makes a campaign harder to
  reproduce. An explicit flag, echoed in the report's reproduction line, keeps the input visible.
- **Carrying unmeasured targets too:** it would close targets whose only failure was an optimizer
  that could not patch them once, which is exactly the case the in-campaign retry exists for.
