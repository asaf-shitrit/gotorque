# 0017. A patch is held to its shape before it is built, and an unmeasured target is retried once

- Status: accepted
- Date: 2026-09-23

## Context

With a code-chosen target (ADR 0013), the optimizer's only job is to write the diff for a remedy
code already picked. Nothing checked that the diff did so before the build, and a candidate that
failed before measurement closed its target the same way a measured verdict did.

On the first live gojq campaign with the Jev analyst, the target was `(*cli).printValues`,
unbuffered I/O at +3.4 sd. The optimizer wrote exactly that remedy, a `bufio.Writer` around the
output with a deferred `Flush`, and left out the `bufio` import. The build failed, the target
counted as tried, and the next cycle moved to a +1.4 sd fast-path target that measured
inconclusive. A candidate slot was spent without the campaign learning whether the remedy works.

## Decision

After the patch is applied and the protected-path check passes, and before the build, the engine
reads Git's zero-context diff of the worktree (`toolchain.ChangedLines`) and rejects the candidate
when:

- a changed Go file does not parse;
- a changed line uses a common standard package (`bufio`, `bytes`, `strconv`, `strings`, ...) that
  the file does not import and the file or package does not declare as a name;
- with a code-chosen target, an existing function other than the target changed (imports,
  package-level declarations and wholly new functions are allowed);
- the target's cause is unbuffered I/O, string building or preallocation, and the added lines lack
  that remedy's mechanism (`bufio.`, and a `Flush` for a writer; a builder, `strconv`, append or
  `Grow`; `make` or `Grow`). The other four causes can be fixed in too many ways to name one, and
  are not judged.

The reason goes to the candidate's failure detail, which the next optimizer call reads in
`prior_candidates`. A candidate rejected before measurement (the patch was invalid, did not apply,
failed this check, or did not build) is marked `unmeasured`, and its target is offered once more;
a second unmeasured attempt closes it. Targets carried across a resume stay closed.

The check only adds rejections. When Git cannot produce the diff, the build decides as before, and
no candidate reaches measurement on the check's say-so.

## Evidence

Replayed against the 13 recorded candidate patches that still apply to their repositories (the
three live Jev campaigns on gron and gojq, and the earlier gron end-to-end runs), the check rejected only
the gojq patch, with "cli/cli.go: the patch uses bufio without importing it; add the import in
the same patch". All 12 patches that built passed: the accepted bufio and `strings.Builder` fixes
of the gron campaigns, and every inconclusive one.

## Consequences

A target the optimizer fails to patch can now cost two candidates instead of one, still within
`max_candidate_patches` and `stop_after_failures`. The import rule knows only the listed packages;
a missing import of any other package is still caught by the build, one step later. The remedy
rules are substring checks on added lines: they can be satisfied by a patch that mentions the
mechanism without using it, which the build, tests and measurement then judge as before.
