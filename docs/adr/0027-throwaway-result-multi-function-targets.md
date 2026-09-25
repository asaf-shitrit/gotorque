# 0027. A throwaway-result signal picks a multi-function target, and the optimizer returns several function sources

- Status: proposed
- Date: 2026-09-26

## Context

Every candidate gotorque has ever built is confined to one function (ADR 0013, ADR 0017): code
chooses a target function and cause, the optimizer's instruction forbids touching anything else, and
the shape check rejects a patch that does. That confinement is what makes `function_source` (ADR
0022) and the shape check tractable, but it also puts a real class of speedup out of reach: one where
the fix spans a callee and its callers, and no single function's diff is the whole story.

dasel (`~/projects/gotorque-work/dasel`, HEAD `c5cf675`) is the recorded case. Profiling ranks
`(*Value).UnpackKinds` as the hottest function in `model/value.go`. It ends every call with `return
NewValue(res)`, a fresh `*Value` allocation. About a dozen of its own package's callers — `IsString`,
`IsInt`, `IsFloat`, `IsBool`, `isStandardMap`, `isDencodingMap`, `IsSlice`, `Append`, `SliceLen`,
`GetSliceIndex`, `SetSliceIndex`, and others — call it only to read a `Kind`, compare a `Type`, or
call one more cheap predicate, then drop the result. A hand-written fix (verified: tests pass, output
byte-identical, json-filter-map -15.6%, yaml-to-json -9.7%;
`~/projects/gotorque-work/recovered-2026-09-25/dasel-audit/fix1-unpack-noalloc.diff`) adds
`unpackKindsValue`, a variant that returns the raw `reflect.Value` instead of wrapping it, and
switches eleven callers to it. Two things stopped gotorque from finding this on its own:

1. Jev classifies a hot function from its own source alone (`internal/jev`, ADR 0012). Handed only
   `UnpackKinds`, it has no way to see that the callers throw the allocation away; ADR 0025's own
   evidence says as much — "dasel's real win was an allocation that the function's callers discard,"
   found by an audit, not by a cause question.
2. The shape check (ADR 0017, `internal/campaign/shape.go`) forbids a patch from touching any
   function but the target. A diff that fixes `UnpackKinds` and switches its eleven callers is, by
   that rule, a diff that edits ten functions too many.

## Decision

**A code-only signal, not a Jev cause.** `internal/campaign/callers.go` finds, for a profiled hot
function, every call to it within the same package directory (same directory as the function's file,
every non-test `.go` file — a method is matched by method name on a selector call, which the
prototype accepts as an ambiguity rather than resolving receiver types), and classifies each call
site:

- **discarded**: the call is a standalone statement, or assigned to `_`.
- **consumed**: the call is the `X` of a selector in the same expression (`v.UnpackKinds(...).Kind()`),
  or is assigned to a local whose every later use in the function is such a selector's `X` — a field
  read or a method call, never stored, returned, passed as an argument, or addressed.
- **escapes**: anything else — returned, stored, passed on.

Separately, `returnsFreshAllocation` asks whether the callee returns a fresh allocation on every path:
every `return` is the address of a composite literal, `new(T)`, or a call to a same-package free
function whose own returns are checked one level deep (composite literal, `new(T)`, or *any*
same-package call, trusted without opening it further). One level is what `UnpackKinds` needs:
its `return NewValue(res)` opens `NewValue`, whose four branches are two composite literals and two
calls (`NewNestedValue`, `NewNullValue`) that are trusted at that second level rather than opened
themselves — `NewNullValue` calls `NewValue` back, so a third level would recurse forever.

**throwaway_result fires** when the callee returns a fresh allocation and at least two call sites, and
at least half of every call site found, are consumed or discarded. The signal is code-derived: it is
never asked of Jev, never added to Jev's question set, and never touches `siteVerdict` or the
measured baseline (`internal/jev/baseline.go`) — `TestBaselineMatchesQuestions` still passes on the
exact digest it always has.

**A multi-function target.** When the signal fires, `internal/campaign/causes.go` builds a target
whose `Cause` is `"throwaway_result"`: the callee plus its consuming callers, ranked by call-site
count. It carries the set as `Target.Functions`, a JSON-encoded `[]FunctionRef` string rather than a
`[]FunctionRef` field, because `agents.Target` is compared with `==` throughout `planTarget`,
`targetKey`, and their tests (`internal/orchestrator/target_test.go`); a slice field would make that
a compile error. `EncodeFunctionSet`/`DecodeFunctionSet` are the wire format.

These targets are put in the **first tier**, ahead of every Jev-ranked target
(`addThrowawayTargets` in `causes.go`, prepended before `analystResult`'s own `Targets`): the
evidence is structural and deterministic — a callee that always allocates, called by sites that
provably only read one field off the result — not a probability estimate that can be wrong the way a
Jev flag can. Everything else about target selection (the deferred fast_path tier, ADR 0025's
vetoes, ADR 0018's discount) is unchanged and still applies to the Jev-ranked targets behind it.

**Cap.** The task that shaped this ADR proposed capping a target's function set at 6. dasel's fix1
alone needs the callee plus eleven callers — twelve functions — to reach the functions the recorded
fix actually edits. This ADR raises the cap to `maxThrowawayFunctions = 12`
(`internal/campaign/callers.go`), matching the budget `causes.go` already uses for `maxCauseSites`
and `excerpts.go` for `maxExcerpts`: twelve was chosen there because it "costs about 24k prompt
tokens, which is noise against a model call that already takes 15s-3m." The same trade-off applies
here. Twelve is still smaller than dasel's full caller list (about eighteen, once every indirect
caller through `IsScalar`, `Set`, and the `*Value`-returning accessors is counted) — a real campaign
against dasel would cover fix1's eleven callers only if the ranking happens to keep them inside the
cap, which is not guaranteed by call-site count alone when most callers tie at one call site each.
**This is a real, open gap**, not a claim that twelve is enough for every case; see Consequences.

**Brief and transport.** The optimizer's brief (`OptimizerBrief`, `internal/orchestrator/orchestrator.go`)
carries excerpts for every function in the set, not just the callee: `addThrowawayTargets` adds a
synthetic `agents.HotPath` per caller location, which the existing, unmodified `extractExcerpts`
(driven entirely by `HotPath.Location`) picks up on its own, and `excerptsAt` (used by `planTarget`)
now keeps the excerpts, and each involved file's header, for every location in `target.Functions`,
not only `target.Location`.

`agents.OptimizerResult` gains `function_sources []string` (ADR 0022's decode leniency style: a
single string is accepted where the list is expected, exactly as `imports` already does via
`flexStrings`). `internal/campaign/multi_function_source.go` builds one multi-file diff from it:

1. Parse and gofmt each source in isolation (`formatFunctionDecl`, reused unchanged from ADR 0022).
2. A declaration whose name matches the callee or a function in the set replaces that function, in
   whichever repository file it is declared in (`groupByFile`, `knownFunctions`) — the set's
   functions may span several files, since a callee's callers are a package-wide search, not a
   single-file one.
3. A declaration whose name matches nothing in the set, and does not exist anywhere else in the
   package (`existsElsewhereInPackage`), is a wholly new function, appended after the callee's own
   declaration in the callee's own file.
4. A declaration whose name exists elsewhere in the package but outside the set is a rejection: it
   would let the optimizer rewrite a function the shape check never agreed to confine.
5. Per file, edits (splices over existing declarations, or an insertion after the callee) are applied
   as a byte-range rewrite processed in descending offset order (`applyEdits`), `addImports` runs
   against the callee's own file only, and `dropOrphanedImports` (ADR 0022, unmodified) runs against
   every touched file.
6. Each changed file is diffed against the base revision (`diffAgainstBase`, reused) and the results
   concatenated into one multi-file unified diff.

`resolveCandidatePatch` picks this transport (`MultiFunctionSourceTransport = "function_sources"`)
only when `patch` is empty and `target.Functions` is set; a lone `function_source` naming the callee
is accepted as a one-element list, the same leniency every other list field on `OptimizerResult`
already has. The ordinary single-function `function_source` path (`target.Functions == ""`) is
untouched — same code, same tests, same behaviour. `patch` still wins over either function-source
transport (ADR 0022's precedence is unchanged).

**Shape check.** `checkMultiConfined` (`internal/campaign/shape.go`) is `checkConfined`'s counterpart
for a multi-function target: it applies the same confinement to every file in the callee's package
directory, not just the target's own file (a file outside that directory is not checked at all,
exactly as the single-function path never confines a file other than the target's own). It needed one
new primitive, `whollyNewDecl`, because `checkConfined`'s own `added()` — requiring every line of a
declaration to be a diff-added line — routinely fails a throwaway_result remedy: dasel's fix1 adds
`unpackKindsValue` by moving `UnpackKinds`' unchanged loop body under a new name and signature, so
most of the new declaration's lines are identical, unmarked context in a zero-context diff, never
added lines at all. `whollyNewDecl` instead checks only the declaration's own signature line (was it
itself changed content?) and that no removed line elsewhere in the file declared a function of the
same name — enough to tell "this name is new" from "this name's body was edited in place" without
needing every body line to be new text. `checkConfined` and `added()` are untouched; this is a new
function for a new code path, not a loosening of the existing one, and the CLAUDE.md rule that the
shape check may only add rejections held throughout: `whollyNewDecl` makes `checkMultiConfined`
*accept* a legitimate patch it would otherwise wrongly reject, which is a correctness fix inside a
new check, not a widening of what the pre-existing single-function check accepts.

## Evidence

`internal/campaign/callers_test.go`'s `TestThrowawaySignalFiresOnConsumingCallersOnly` is the unit
fixture: a callee returning `NewX(...)` with four callers, three consuming and one escaping — the
signal fires and the caller set is exactly the three consuming callers.
`TestThrowawaySignalRequiresAFreshAllocation` and `TestThrowawaySignalNeedsAtLeastTwoConsumingSites`
cover the two gates that keep it from over-firing.

`internal/campaign/dasel_replay_test.go` is gated on `~/projects/gotorque-work/dasel` existing (`t.Skip`
otherwise, so CI, which never has the clone, skips cleanly) and checks, against the real source, read
only:

- `TestThrowawaySignalFiresOnDaselUnpackKinds`: the signal fires on `(*Value).UnpackKinds`, and the
  caller set the (uncapped) analysis finds is a superset of every function fix1 edits (`IsString`,
  `IsInt`, `IsFloat`, `IsBool`, `isStandardMap`, `isDencodingMap`, `IsSlice`, `Append`, `SliceLen`,
  `GetSliceIndex`, `SetSliceIndex`).
- `TestShapeCheckAcceptsFix1UnderTheFullCallerSet`: builds a scratch git repository from dasel's
  `model` package (never the clone itself, which the test only reads), applies fix1 with plain
  `git apply`, and confirms `checkShape` accepts it when the target's function set is that full
  caller list. `isFloat` and `isSlice`'s deletion by the same diff needed no special handling —
  `checkFile` parses the *post-patch* file, so a deleted function has no surviving declaration to
  flag in the first place; this was verified, not assumed.

This test deliberately uses the *uncapped* caller list, not the twelve-function production cap: see
Consequences for what that means for a live campaign.

`internal/campaign/multi_function_source_test.go`'s `TestBuildMultiFunctionSourceDiffAppliesAndBuilds`
is the end-to-end transport test: four function sources (a brand-new helper, the callee rewritten to
use it, and two callers in a different file switched to it) become one multi-file diff, which applies
with plain `git apply` and then `go build ./...`s clean.
`TestBuildMultiFunctionSourceDiffRejectsANameOutsideTheSet` covers the rejection case.

`internal/campaign/shape_multi_test.go` covers `checkMultiConfined` directly: an edit across the
set's two files is accepted, an edit to a same-file function outside the set is rejected, and a file
in an unrelated package directory is never confined.
`internal/orchestrator/target_test.go`'s `TestPlanTargetKeepsExcerptsForEveryFunctionInASet` covers
`excerptsAt`'s extension, and `internal/campaign/causes_test.go`'s (via `callers_test.go`)
`TestAddThrowawayTargetsPrependsFirstTier` covers the `causes.go` wiring.

`make lint` (0 issues), `GOCACHE=/private/tmp/gotorque-cache go test ./...`, and
`make cover && make crap-check` (0/885 functions over CRAP 30) all pass on this branch. An
`--adk-stub` run against `~/projects/gotorque-work/gron` completes end to end with two extra hot
paths reaching the excerpt collector, confirming the wiring runs without a live model or network call.

## Consequences

A hot function whose real cost is a callee-and-callers allocation pattern can now be targeted at all,
which no prior ADR made possible — the confinement that ADR 0013/0017 rely on for every other cause
stays exactly as strict, applied to a set instead of one function.

The twelve-function cap is a real limitation, demonstrated rather than assumed: dasel's own fix
needs all twelve slots, and a package with more legitimate consuming callers than that (dasel's full
list is closer to eighteen, once accessors like `IsNull`, `StringValue`, `Set` are counted) would have
some of them ranked out. Call-site count is a weak ranking key when — as in dasel — almost every
caller ties at exactly one call site; the replay gate exercises the *uncapped* signal for exactly
this reason, and a live campaign's capped target is not guaranteed to reach the same coverage. Raising
the cap further, or ranking callers by something better than raw count (e.g. how much of the excerpt
budget they would cost, or a second structural signal), is future work this ADR does not attempt.

`function_sources` inherits `function_source`'s risk surface: a misnamed declaration, a parse
failure, or a declaration for a function outside the set is a pre-build rejection (ADR 0017's retry
policy), same as before, just now with more ways to name the wrong thing across more files.
`addImports` still only reaches the callee's own file; a caller in a different file that needs a new
import of its own is not supported by this prototype and would fail to build, caught by the ordinary
build gate one step later, same as an unsupported import always has been.

Method matching by name alone (not by receiver type) can conflate two unrelated methods that happen
to share a name across different receivers in the same package directory — a false-positive risk this
ADR accepts and documents rather than resolves, since resolving it needs type information the rest of
`internal/campaign`'s AST-only analysis does not carry.
