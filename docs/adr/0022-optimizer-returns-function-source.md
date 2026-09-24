# 0022. With a code-chosen target, the optimizer returns the function's new source, and code builds the diff

- Status: accepted
- Date: 2026-09-25

## Context

ADR 0013 lets code choose each candidate's target function and cause. Once a target is chosen, the
optimizer's whole job is to write a diff that implements the target's remedy in that one function.
Across recent live campaigns, the most common way a candidate was lost before measurement was diff
mechanics, not the mechanism itself: a hunk whose context line did not match because the model
paraphrased a line instead of copying it, a bare `--- cli/cli.go` header GNU patch resolved to the
wrong file, a hunk anchored to the wrong place in a 400-line file, or a hunk that carried the fix
but not the import it needed. Every one of these is a model editing text that has to align
byte-for-byte with source it can only approximate from what it was shown, for a target code had
already located precisely.

The fixes so far treated the symptoms: `internal/candidate/remap.go` recovers a header GNU patch
misread, `--unidiff-zero` (`internal/toolchain`) stopped git anchoring a context-free hunk at the
wrong end of the file, the patch-shape check (ADR 0017, `internal/campaign/shape.go`) catches a
missing import before the build does, and the excerpt collector was extended to include a file's
header so an import hunk has something to anchor to. Each closed one failure mode. None of them
address the shared cause: with a target, the optimizer is not proposing where to change source, only
what a function should become, and a unified diff is the wrong wire format for that.

## Decision

`agents.OptimizerResult` gains two optional fields: `function_source`, the complete new declaration
of the target function (doc comment optional), and `imports`, the import paths it needs that the
file does not already have. The optimizer's instruction tells it: with a target, return
`function_source` and `imports` instead of hand-writing `patch`. `patch` keeps working exactly as
before for the no-target path, and remains an accepted fallback even with a target — an optimizer
that ignores the instruction still produces something the deterministic gates can judge. Decoding is
as lenient as every other field (`internal/agents/decode.go`, `fence.go`): `function_source` accepts
a wrapper object the way `patch` does, `imports` accepts a single string the way `risks` does. None
of this touches what a value means once decoded; it only widens what parses.

Deterministic code (`internal/campaign/function_source.go`) turns `function_source` into an ordinary
unified diff before anything downstream sees it:

1. Read the target's file at the campaign's base revision — the canonical checkout, clean at that
   revision for the duration of a candidate's evaluation.
2. Parse it and find the `FuncDecl` whose name matches `target.Function`, in the same
   path/receiver-qualified format `internal/campaign/causes.go`'s `funcName` already produces
   (`"(*cli).printValues"`, `"gron"`). No match is a rejection naming the function and file.
3. Parse `function_source` on its own and require it to be exactly one function declaration; more
   than one, zero, or a parse failure is a rejection. A declaration whose name (and receiver) does
   not match `target.Function` is rejected too — the same confinement the shape check already
   enforces on a hand-written patch, moved earlier.
4. gofmt the new declaration in isolation, then splice it over the old one's byte range. The old doc
   comment is kept when the new source carries none of its own; it is replaced when the new source
   has one.
5. Add any `imports` paths the file does not already have to the first group of its import block
   (where Go files keep the standard library), or to a new block, and gofmt that declaration alone.
   gofmt sorts within a group and never across the blank line between groups, so the file's
   standard-library and third-party groups and every other import line stay as they were.
6. Diff the old and new file contents with `toolchain.DiffFiles`, a typed wrapper around
   `git diff --no-index` whose a/ and b/ headers are rewritten from the scratch paths it used to the
   real repository-relative path. `git diff --no-index` exits 1 when the files differ; that is the
   expected outcome here, not a failure, so only other nonzero exits are reported as errors.

The produced diff goes through the unchanged pipeline: `NormalizeUnifiedDiff`, `ValidateUnifiedDiff`,
worktree apply, the patch-shape check, build, test, measure. None of those steps can tell a
function-source diff from a hand-written one, which is the point: the transport changes how the diff
was produced, not what judges it. `domain.Candidate` gains a `Transport` field (`"patch"` or
`"function_source"`), set once in `evaluateCandidate` and carried through `prepareCandidate`'s
overwrite of `evidence.Candidate`, so `CandidateRecord` and the Markdown report can say which one
produced an attempt.

A failure at any step (function not found, unparseable or misnamed `function_source`, a diff that
comes out empty) is a pre-build rejection: `Unmeasured` is set, `FailureDetail` carries the reason,
and the target is offered once more (ADR 0017's retry), exactly like a hand-written patch that fails
`git apply --check` or the shape check.

Only `patch` takes precedence when both are set; without a target, `function_source` is never
consulted, so an optimizer that sends one anyway (there is nothing stopping a model from ignoring the
"no target" case) has no effect — the no-target path is byte-for-byte what it was before this ADR.

## Evidence

`internal/campaign/function_source_test.go` covers replacing a method and a plain function, keeping
the old doc comment when the new source has none and replacing it when it does, adding a missing
import to an existing import block and to a file with none, rejecting a `function_source` that
declares a different name, and rejecting one that does not parse or declares more than one function.
`TestBuildFunctionSourceDiffAppliesCleanly` drives the whole builder end to end and applies the
result with plain `git apply`, byte-comparing the resulting file. `TestEvaluateCandidateBuildsFromFunctionSource`
is an engine-level test: a proposal carrying `function_source` and a target reaches build with a
correctly assembled patch. `internal/orchestrator/function_source_test.go` pins that the orchestrator
graph still forwards `patch`, `function_source`, and `imports` verbatim to `CandidateRequest`,
whether or not a target is present — transport resolution is `internal/campaign`'s job, not the
graph's. `internal/toolchain/diff_files_test.go` checks `DiffFiles` rewrites its headers to the
repository-relative path rather than leaking the scratch directory, and that the diff it produces
applies with `git apply` to a real repository. All of these tests were written to fail against the
pre-ADR code (the functions and fields they exercise did not exist) and pass against this change.

## Consequences

A target the optimizer addresses through `function_source` can no longer fail on context-line drift,
a misread header, or a wrong anchor, because none of those exist in this transport — there is no
hunk to mismatch. The bar moves entirely to whether `function_source` is the function the optimizer
meant to write; `checkConfined` and the remedy-shape checks in `internal/campaign/shape.go` still
judge that, unchanged, because they read the diff Git produced, not how the diff was produced.

A new import always goes into the first group. A file that keeps a third-party package in its first
group and the standard library later would get the import next to the third-party one: valid and
building, but not where a maintainer would put it.

`function_source` only makes sense for a code-chosen target: it needs a function name to find. The
no-target path (an optimizer choosing its own site) is exactly as before, and cycles that exhaust
every target still fall back to the optimizer's full-state view and `patch`, unchanged by this ADR.
