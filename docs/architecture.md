# Architecture

## Responsibility split

Google ADK Go 2.x is the internal orchestration runtime. It does not
reimplement deterministic Go, Git, profiling, coverage, or statistical tools.

```text
CLI
    |
    v
in-process campaign engine
    |
    v
ADK workflow graph -------- campaign state and artifacts (bbolt)
    |
    +-- deterministic nodes: inspect, discovery, evaluate candidate, policy, routing
    |
    +-- specialist agents: coordinator, explorer, analyst, optimizer, reviewer
```

Campaign state lives in a bbolt store (`campaign.db`) inside the campaign
directory. State is saved before every event append, so an interrupted run
resumes from the last persisted step. Artifacts (run stdout and stderr,
coverage files, profiles, patches, benchstat samples) are content-addressed
under the campaign directory.

## ADK workflow

The workflow is a bounded graph rather than an unconstrained chat loop. The
actual node sequence built by `internal/orchestrator` is:

```text
initialize_campaign
  -> inspect_repository
  -> coordinator (choose next experiment)
  -> explorer (propose workload strategies; with --explorer jev, a stub that
              reports the variants discovery already sampled)
  -> run_discovery (deterministic; validates each proposal)
  -> analyst (interpret profile and coverage evidence; with --analyst jev,
             deterministic Jev cause classification instead of a model)
  -> merge_analysis (deterministic; attach source excerpts)
  -> optimizer (one focused patch)
  -> evaluate_candidate (deterministic; see below)
  -> reviewer (adversarial review, advisory only)
  -> apply_policy (deterministic acceptance decision)
  -> route_campaign
        continue -> back to coordinator
        finish   -> finalize_campaign
```

The route node stops the loop when the manifest's maximum candidate count or
consecutive-failure limit is reached; the engine's `max_duration` bound (see
Campaign bounds) can also end a run from outside the graph, at whatever node
is executing when it expires.

A role whose model call fails degrades to an empty result (`role_degraded`)
instead of ending the run, because every role has a deterministic fallback
for an absent answer. A provider that is down, or a revoked key, used to
exploit that: every role degraded on every cycle, the optimizer's empty patch
was rejected as if a model had written it, and the campaign ended
`completed` at the rejection streak, blaming the patches for the provider.
Each degraded role now appends to `CampaignState.CycleFailures`, and route
checks it before any bound. When every model role failed in the same cycle,
the campaign stops with a stop reason naming the last failure, and
`finishCampaign` returns `ErrProviderUnavailable`, so it ends `failed` and can
be resumed once the provider answers. The record is cleared every cycle, so
roles that fail in different cycles never add up to an outage. A role Jev
serves is not counted: with `--analyst jev` neither the analyst nor the
coordinator, which becomes a deterministic plan that never calls a model, with
`--reviewer jev` not the reviewer, and with `--explorer jev` not the explorer,
a stub that reports the variants discovery sampled. Jev is served by a different gateway,
so a role it answers would otherwise keep the breaker from ever tripping. When
those roles leave the optimizer as the only model role, one cycle in which it
failed is the same single failed call the degrading wrapper absorbs whenever
another role answers, so the breaker then waits for two consecutive failed
cycles (`outageCycles`). One slow cycle had ended a gojq campaign two
candidates early. Permanent HTTP statuses are still not retried, so a revoked
key stops a campaign within two quick cycles.

The final acceptance transition is
always produced by deterministic policy (`internal/policy`); agent output,
including the reviewer's recommendation, never decides acceptance by itself.

## Candidate evaluation loop

`internal/campaign/candidate_eval.go` runs the deterministic half of each
candidate cycle. The model never self-approves: every terminal judgment is
produced here or in policy.

0. **Transport resolution (ADR 0022).** With a code-chosen target, the
   optimizer may return `function_source` (the target function's complete new
   declaration) and `imports` instead of a hand-written `patch`.
   `resolveCandidatePatch` in `internal/campaign/function_source.go` chooses
   which the rest of the loop sees: a non-empty `patch` always wins (it is
   also the only transport when there is no target), otherwise a target plus a
   non-empty `function_source` builds the diff deterministically. Building
   parses the target's file at the base revision, finds the `FuncDecl` named
   `target.Function` (the same format `causes.go`'s `funcName` produces),
   splices the gofmt'd new declaration over it (keeping the old doc comment
   unless the new source carries one), adds any missing imports, drops any
   import the replacement took the file's last use of, and diffs the
   result against the original with `toolchain.DiffFiles`
   (`git diff --no-index`, headers rewritten to the repository-relative path).
   A step that fails — function not found, `function_source` that does not
   parse as exactly one matching declaration, an empty diff — is a pre-build
   rejection, `Unmeasured`, exactly like a hand-written patch that fails
   `git apply --check`. What reaches step 1 is always a unified diff; nothing
   past this point can tell which transport produced it, which is the point.
   `domain.Candidate.Transport` records which one did, for the report.
1. **Diff validation and normalization.** The proposed unified diff is
   normalized by `internal/candidate/normalize.go`, which repairs hunk line
   counts, truncates hunks at the first malformed body line, drops emptied
   hunks with their file headers, and canonicalizes the trailing newline.
   It also restores blank context lines. A unified diff writes a blank source
   line as a single space, and that lone trailing space is the first thing
   lost when a diff crosses a model, a JSON string, or anything that trims
   line ends; ending a hunk at every empty line truncated genuine patches at
   their first blank source line, and when the blank fell on the first body
   line the hunk emptied out, its file headers went with it, and the campaign
   rejected a valid candidate as a malformed diff. A hunk body is contiguous,
   so an empty run with more body lines after it can only be blank context,
   while an empty run at the end is trailing slack and still ends the hunk.
   `ValidateUnifiedDiff` then rejects empty or oversized patches, binary
   content, diagnostic instrumentation files, and, depending on the
   manifest's optimization policy, prohibited techniques such as `unsafe`,
   assembly, or cgo. It checks every name git apply or patch could act on:
   `---`, `+++`, `diff --git`, rename/copy, `***` and `Index:` headers, each as
   written and as `-p1` strips it, deletions to `/dev/null` included. Paths
   that escape the repository are rejected, and so are protected paths:
   `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `default.pgo`, `vendor/`,
   `*_test.go`, `testdata/`, `.git` and `.gitattributes`. Mode, symlink,
   submodule and git-binary changes are refused too. The test files are on
   that list because they define the gate in step 4: a patch that can edit a
   golden file or skip an assertion can make itself pass. Checking only `+++`
   paths let a deletion (`+++ /dev/null`) or a rename past the dependency
   rule.
2. **Isolated worktree at the base revision.** A Git worktree is created from
   the recorded base revision under the campaign directory. The normalized
   patch is applied with strict `git apply --check`; when that fails because
   model context lines are approximate, GNU patch with `--fuzz=5` is tried as
   a fallback. patch picks its target file by its own rules, not by the
   header validation read: with `--- a/go.mod` / `+++ b/other.go` it edits
   `go.mod`, and it follows a tracked symlink into `testdata/` that git
   apply refuses. So once the patch is applied, `toolchain.ChangedFiles`
   lists every path Git sees as changed in the worktree, ignored and
   untracked files included. A protected path or a new symlink rejects the
   candidate before build, with the path named in its failure detail. Apply
   errors are returned with captured stderr so the optimizer can see why its
   diff was rejected.
   The applied change is then held to its shape (ADR 0017,
   `internal/campaign/shape.go`), read from `toolchain.ChangedLines`, Git's
   zero-context diff of the worktree. Every changed Go file must parse; a
   selector on a common standard package (`bufio.`, `strings.`, `strconv.`...)
   on a changed line must be imported, unless the file or package declares
   that name. With a code-chosen target, no existing function other than
   the target may change; imports, package-level declarations and wholly new
   helpers may. And for the three causes whose remedy has one recognisable
   mechanism, the added lines must carry it: a `bufio.` reader or writer
   (flushed, when it is a writer) for unbuffered I/O, a `strings.Builder`,
   `bytes.Buffer`, `strconv.`, `append(` or `Grow(` for string building, a
   `make(` or `Grow(` for preallocation. A failure rejects before build with
   the reason as failure detail. A target function that wraps a writer in a
   `bufio.Writer` must also route every write through it: a direct write left
   on the wrapped writer lands before the buffered output and reorders it,
   which is how a gojq patch that buffered `printValues`' values but not its
   newlines reached the test gate. The check can only add a rejection: when Git
   cannot produce the diff, the build decides as before.
3. **Release build.** The patched tree is built with release-equivalent flags
   into the campaign builds directory. Build failures end the attempt with
   the compiler stderr attached to the candidate record. When
   `target.build.directory` names a nested Go module (ADR 0023, e.g.
   `alecthomas/chroma`'s `cmd/chroma`, which keeps its own go.mod with a
   `replace ../../`), the build runs with that directory as `go build`'s
   working directory and `target.build.package` resolves relative to it;
   every build in the campaign — baseline, its coverage twin, every
   candidate, and the PGO lane's baseline/candidate pair — does the same.
   Left unset, a build runs from the repository root exactly as before ADR
   0023. The upstream test-suite gate below and discovery's benchmark
   profiling stay root-module-only regardless.

   A candidate that fails in steps 1-3 is recorded as `unmeasured`: it says
   nothing about its target, so `planTarget` hands the same target to the
   next cycle, with the reason in `prior_candidates`. A second unmeasured
   attempt closes the target, so one the optimizer cannot patch does not
   hold the campaign.
4. **Upstream test-suite gate.** `go test -json` must not regress against the
   unpatched revision before measurement starts: the campaign runs the
   target's own suite once during baseline discovery, records the failing
   tests it finds there (`baseline_test_failures`), and rejects a candidate
   only for failures absent from that set. Without the subtraction an
   environment-drifted suite rejects everything — on Go 1.27 gojq's tests
   assert an `encoding/json` error string a later release changed, so no
   patch could ever have passed — and a test failure that predates the patch
   is evidence about the operator's toolchain, not about the candidate. A
   baseline run whose tests never executed at all (build or setup failure)
   stops the campaign instead, because subtracting it would leave the gate
   switched off while still reporting verdicts. The baseline run also
   records every test that passed, subtests included
   (`baseline_test_passes`). Comparing failures alone never noticed a
   baseline-passing test that the candidate run skipped or never ran, so a
   test disabled by the patch passed the gate. Every candidate run is now
   classified, clean exits included, and a candidate is rejected when any
   baseline-passing test fails, skips, or does not run. The rejection is
   made without any timing comparison, and the report names the tests. State
   written before the pass set existed re-runs the baseline step once
   (`CompletedSteps["baseline_test_passes"]`) instead of running the rest of
   the campaign without the check. The baseline suite runs in the canonical
   checkout and candidates run in fresh worktrees. A test that passes only
   because of an ignored local file therefore rejects every candidate, by
   name.
5. **Interleaved A/B measurement.** For each representative-tier seed
   workload, baseline and candidate binaries are measured in serialized
   alternating pairs (baseline first, twenty-five pairs per workload) so CPU
   contention affects both sides equally. The pair count is sized against the
   noise of short CLI workloads: a target that runs for 10-25 ms per seed
   carries 6-8% per-run spread, and at seven pairs a real 3.75% win measured
   as unsupported while thirty pairs resolved the same effect at p<1e-4.
   Before the pairs run, the baseline
   is executed twice against itself: if two identical runs produce different
   stdout digests, the workload is treated as nondeterministic and behavior
   comparison switches to an order-insensitive sorted-lines digest, so
   cosmetic row ordering cannot reject a behavior-preserving patch.
   Exit codes must match in all cases. When an eligible reading (below)
   regresses past the limit without significance, every representative
   workload is measured over a second series of twenty-five pairs, held to
   the same behavior check, and every comparison is derived again from both
   series (`confirmRegressions`, ADR 0016). All workloads are extended, not
   only the one that read high, because the pooled reading folds them per
   repetition. The second series has no budget of its own: it costs what
   the first did, and the campaign deadline bounds both. The same thing
   happens on the acceptance side: when the candidate would otherwise end
   inconclusive only because an eligible reading improved by at least
   `minimum_improvement_percent` without statistical support, and nothing is
   about to reject it, every workload is measured again the same way
   (`confirmImprovements`, ADR 0021). At most one extra series runs per
   candidate in total — if the regression confirmation already extended
   every seed, the improvement check reuses that series instead of running a
   third.
6. **Statistics.** Each metric gets a two-sample Welch t-test against a
   conservative critical value (`|t| > 2.2`, roughly p < 0.05 for these
   sample sizes); support is never reported from fewer than four samples per
   side. When the optional `benchstat` binary is installed
   (`internal/campaign/benchstat.go`), raw wall-time samples are written per
   workload and benchstat refines the result: a parseable p-value below 0.05
   grants support, a parseable but insignificant p-value withdraws support
   the coarse t-test may have granted, and delta-only legacy output is
   informational and can never grant support by itself. Each comparison
   also records `significant`: benchstat's p below 0.05 when it ran,
   otherwise `|t| > 2.2`. That is not the same as support, which is also
   granted to a flat reading whose interval rules out a 2% regression, and
   it is what a regression is judged on. Trimmed benchstat
   output is kept in the candidate record for reports. Representative
   workloads are folded per repetition before the pooled comparison: the
   mean across workloads for wall and CPU time, the maximum for peak memory,
   which is a high-water mark rather than an additive quantity. Concatenating
   raw samples from workloads of different scale instead inflates the pooled
   spread with between-workload variance: on gron a real 20.8% win on the
   large workload produced a pooled `t` of 1.06 against 10.69 measured on the
   affected workload alone, and the candidate was reported inconclusive.

### What a verdict rests on

Acceptance is decided over the set of **acceptance-eligible readings**: the
pooled primary metric plus one comparison per representative-tier seed (only
those seeds are measured, so every per-workload primary comparison is eligible
by construction). A candidate passes when any member of that set improves by
at least the manifest's `minimum_improvement_percent` with statistical
support, and no member regresses significantly past
`maximum_guardrail_regression_percent`;
the guardrails themselves (`peak_memory_bytes`, `cpu_time_ns`,
`binary_size_bytes` by default) are checked on the pooled comparisons as
before. The reason names the workload the verdict rests on.

Those limits come from the manifest's performance block unless the campaign
was started with a trade-off (`--tradeoff speed|balanced|lean`, `--allow
metric=percent`, ADR 0020). `manifest.Tradeoff.Apply` resolves it once, in
`Create`, into the campaign's copy of the manifest: `policyConfigFromManifest`
is the only reader of the performance block, so verdicts, the confirmation
series and resume all see the same limits with no second code path.
`State.Tradeoff` records where they came from, and the report's "Judged under"
line states them. ADR 0020 left discovery targeting a manifest's memory
objective as an open limitation: discovery profiled CPU regardless, so `lean`
found memory wins only where CPU-hot code also allocated. ADR 0024 closes it
(see the Jev cause analyst and discovery sections above): the resolved
objective ranks allocation causes first and, when the module has benchmarks,
adds a benchmark `alloc_space` profile's hot functions to discovery's list.

Eligibility is encoded in the comparison, not in its name. A
`domain.MetricComparison` carries the canonical `metric` plus the `workload` it
was measured on, where an empty workload *is* the pooled reading. That replaced
a string convention — `<workloadID>/<metric>` for a single workload, the bare
metric for the pool — which the engine built, the campaign re-parsed to decide
eligibility, and the policy parsed again to name the workload in its reason.
Three packages had to agree on one format, and the workload it printed was the
derived run identifier rather than the seed id an operator writes in the
manifest. Comparisons and sample rows now carry the manifest seed id, so a
verdict reads `workload "flatten-users" improved by 4.40%`; policy still holds
every decision and still touches no filesystem, process, or network.

A regression over the limit rejects only when it is significant, if the
manifest asks for statistical support (ADR 0016). On the point estimate
alone, a no-op patch measured against itself on gron was rejected in 12 of
30 trials: bursts of slow runs moved a 25-pair mean past 2% on a seed whose
median had not moved, and a live campaign lost a -20.4% supported win to a
small-doc reading of +4.01% benchstat did not find significant. Before a
reading reaches the policy unresolved, the engine measures again (step 5),
so a real regression 25 pairs could not resolve still has more samples to
show up in. Under this rule the same 30 trials rejected one, on a reading
benchstat called significant, and accepted none. A reading that stays over
the limit without significance does not reject, and the verdict names it
(`workload "small-doc" read +4.01%, over the 2.00% limit, but the difference
is not statistically significant`). `policy.UnconfirmedRegressions` is the
rule both the engine and the verdict use, so what the engine measures again
is exactly what the policy would otherwise wave through.

The same noise floor works the other way: an eligible reading that improved by
at least `minimum_improvement_percent` but lacks statistical support leaves the
candidate inconclusive rather than accepted, and 25 pairs often cannot resolve
a 5-10% effect on a short CLI workload — three live yq candidates read 11.55%,
9.51% and 5.39% improvements that were discarded this way. When the candidate
would otherwise end inconclusive only for that reason, with nothing about to
reject it, the engine measures every representative workload again the same
way it does for a regression (`confirmImprovements`, ADR 0021), using
`policy.UnconfirmedImprovements` as the mirrored predicate. At most one extra
series runs per candidate: if the regression confirmation already extended
every seed, the improvement check does not run a second.

The pooled figure alone was the wrong instrument. A seed whose measured run is
mostly process startup cannot be improved by any patch, so pooling it dilutes
real wins toward zero: on gron the large-document seed improved 3.35% with
support (benchstat p=0.006) while the 41-byte seed sat at -0.81% unsupported,
which pooled to -2.53% against a 3% bar — a supported win reported as
inconclusive. The eligible set is supplied by the engine, which owns the
manifest and the tier rules; `internal/policy` still holds every decision and
still touches no filesystem, process, or network.

The acceptance criteria themselves come from the target's manifest
(`performance.minimum_improvement_percent`, `maximum_guardrail_regression_percent`,
`primary_metric`, `guardrails`, `statistical_support_required`). They used to
be loaded, validated, and then ignored, because the policy was handed
`policy.DefaultConfig()`; the defaults now fill in only what a manifest leaves
out.
   Folding leaves the reported delta unchanged, because dividing every sample
   by a constant leaves a Welch t unchanged.
7. **Policy.** `internal/policy` applies the fixed verdict order: behavior
   and safety failures are hard rejections; missing evidence or a primary
   metric that is not statistically supported or improves less than 3 percent
   is inconclusive; any guardrail (CPU time, peak memory, binary size)
   regressing more than 2 percent rejects, and so does an eligible reading
   that regresses past the limit significantly. Statistical support is required of
   the primary metric, where it protects the win itself; a required guardrail
   is judged against its own `maximum_regression_percent` limit, because
   demanding proof of the absence of a regression from a jittery high-water
   mark reports inconclusive for a candidate that moved it by a fraction of
   the limit. Every verdict is persisted with
   reasons and metric comparisons. An evaluation that never reached a
   behavior comparison carries a `FailureSummary` naming what actually
   happened (the patch failed to apply, the build failed, the upstream test
   suite failed) and policy reports that instead of the behavior-mismatch
   reason, which would misdescribe the rejection to anyone reading a report.

## Source-excerpt enrichment

After the analyst reports hot paths, `merge_analysis` calls
`internal/campaign/excerpts.go` (best effort, deterministic). The candidate
locations are the analyst's hot paths followed by discovery's own measured
locations (`discovery_hot_functions`, marked `measured during discovery`), so
a turn in which the analyst reports nothing resolvable still yields excerpts
from the profiler's evidence. Locations of the form `path.go:line` are
resolved to real source windows: up to 120 lines starting 40 lines before the
target line, capped at 8 KiB each and a total budget of twelve full windows
(96 KiB), which is why the constants read `maxExcerpts * maxExcerptBytes`
rather than two independent numbers. Five windows was the limiter the
optimizer actually hit: discovery measures 15-29 hot functions per target, so
a five-window budget showed the model about a fifth of the hot path and every
candidate could only attack the one frame it could see.
Locations that are absolute, escape the repository root, repeat a
`path:line` already taken, or do not resolve to readable files are skipped
silently. The cap counts excerpts actually produced rather than locations
examined: capping candidates first meant five unusable leading locations
yielded nothing even when later ones resolved cleanly. The excerpts travel in campaign state as `source_excerpts`, and
both coordinator and optimizer instructions direct the optimizer to anchor
diff context lines to excerpt text rather than guessing, which is what keeps
strict `git apply` viable on model-generated patches.

## Jev cause analyst

`--analyst jev` (with `--adk` or `--adk-stub`) replaces the analyst model role
with cause classification ranked in code (ADR 0012). The orchestrator swaps the
analyst agent node for a function node of the same name that calls
`Dependencies.Causes`; `internal/campaign/causes.go` implements it and
`internal/jev` holds the gateway client, the questions, their baseline, and the
ranking. The node returns the same `AnalystResult`, so `merge_analysis`,
excerpts, and the optimizer are unchanged, and a failure degrades like a failed
model call: an empty result, a `role_degraded` record against `analyst`, and
discovery's hot paths as the fallback. Only a cycle in which no function could
be classified fails the node; a single function that fails is listed in
`additional_checks` instead.

Jev (TypeSafe, reached through the Vercel AI Gateway's native `/v1/evaluate`,
which the OpenAI-compatible API does not serve) answers typed questions about a
state with calibrated probabilities and generates no text. For each of up to
twelve discovery hot functions with a source position, one per function, the
node sends the whole declaration with its doc comment and seven yes/no
questions, one per cause: avoidable allocation, unbuffered I/O, string
building, a missing fast path, superlinear work, missing preallocation,
repeated work. Three choices come from a benchmark of 93 real single-function
performance fixes, each asked about before and after the fix:

- The state is the function and its file, never the profile. With the gron
  benchmark profile beside the source, Jev labelled an unbuffered `Fprintln`
  loop and a per-call `bytes.Buffer` as lookup cost, because that profile was
  dominated by unicode-table lookups; without it both were named correctly.
  The profile has already done its job by choosing which functions to ask
  about.
- One yes/no question per cause rather than one "which cause" choice. The
  choice question was the weakest way to name a cause and the only format a
  misleading profile derailed; per-cause Scores on a shared scale lost the
  ability to tell fixed code from unfixed.
- Answers are compared against Jev's usual answer to each question, not raw.
  Its mean yes runs from 0.10 (unbuffered I/O) to 0.53 (allocation). With
  the current questions, raw answers and baseline z name the right cause
  about equally often (44% and 42% top-1 on the 93-fix benchmark; raw puts it
  in the top two more often, 78% against 62%), but z is what the flag rule
  needs: under it the ordering key makes no measurable difference, raw
  ordering over-picks allocation (55% of top picks against 38% of labels),
  and a raw 0.5 floor without z nearly doubles flags on fixed code (0.22
  against 0.12). The study is in `gotorque-work/jev-z-vs-raw.md`; the
  36%/50% figures quoted here before came from an earlier question wording.
  A cause is flagged when it stands +0.5 sd
  above that usual answer *and* Jev's own probability is at least 0.5, two at
  most per function (ADR 0025). The z gate alone flagged the fixing commit's
  cause on 59% of unfixed functions and on 23% of the fixed versions; the
  probability floor exists because z alone did not separate winners from
  losers in live campaigns, where every accepted fix had p >= 0.67 and the
  highest z-scores, all fast_path, never led to one. A cause that fails either
  gate, or a code veto below, is skipped rather than ending the list, so a
  lower cause that clears them still gets the slot. fast_path is kept but
  deferred: its targets form a final tier after every other cause.
- Each question names the state field it judges (`` `source` ``), as
  TypeSafe's guidance asks because Jev reads literally; that measured neutral.
  Three questions judge a relationship rather than one fact (allocates per
  call *and* could avoid it; general path *and* cheap check possible; grows
  *and* size known). Splitting them into one fact per question, as the Noul
  guidance suggests for independent conditions, cut fix detection from 0.49
  to 0.38: each part describes something that survives the fix, so only the
  relationship separates broken code from fixed code. Structured criteria
  with examples gave no net gain.

`internal/jev/baseline.go` holds that baseline, measured over the benchmark's
186 functions with no labels involved. It is valid only for the exact question
text and state template: `TestBaselineMatchesQuestions` compares a digest of
both and fails on any edit, and `TestLiveBaseline` re-measures it (opt-in, one
request per function). It is also valid only for the model version it was
measured on, and nothing can check that: TypeSafe advises pinning a version
once thresholds are tuned against it, but the gateway serves only the alias
`typesafe-ai/jev` (pinned IDs such as `jev-1.13.0` return 404) and reports no
version in its responses. The baseline was measured on Jev 1.13; re-run
`TestLiveBaseline` when TypeSafe ships a release.

Each flagged cause becomes a one-sentence remedy in `candidate_hypotheses`,
every function's first cause before any second cause, each tier ordered by
the cause's z divided by the square root of one plus its function's hotness
rank (ADR 0018), and a
function with nothing flagged is named in `additional_checks` rather than
guessed at. When the campaign's resolved objective is `peak_memory_bytes`
(`--tradeoff lean`, ADR 0020), each tier is additionally split in two before
that z/sqrt(1+rank) sort: `jev.Cause.IsAllocation` causes (`alloc`,
`prealloc`, `string_build`) lead every other cause, the discounted order kept
inside each half (ADR 0024). The objective reaches the node on
`CauseRequest.Campaign.Objective`, set once in `Engine.campaignRequest` from
the manifest already resolved by `Tradeoff.Apply`, so this never re-reads the
trade-off flags.
Code vetoes two causes whose mechanism it can see in the source, before
flagging (`internal/campaign/veto.go`, ADR 0025). unbuffered_io is vetoed
unless some read or write in the function reaches a real file or stream: a
print to the process's own streams, or a call whose destination is not
provably an in-memory buffer. Destinations are resolved from the receiver's
struct fields, the parameters and local declarations as `onlyInMemoryIO` does,
and one it cannot resolve counts as real I/O. Jev reads the source alone, so
it cannot tell `e.w.WriteByte` on a `*bytes.Buffer` field from a write to a
file; on gojq it flagged `(*encoder).writeByte` for unbuffered I/O at +2.9 sd,
and two of a campaign's three candidates went to a function that makes no
system call. prealloc is vetoed unless something grows inside a loop: an
append or map write to a container the function did not make with a size
(`make([]T, 0)` still grows). A function the parser cannot find is never
vetoed. Vetoed causes, and causes skipped for Jev's own probability, are named
under `overruled` in the `cause_analysis` event. The nine fix-kind questions
(ADR 0019; fast path was turned off after it did worse than guessing on
held-out fixes) are batched into the same request as the seven cause
questions rather than sent as a second one (ADR 0028): `combinedQuestions` in
`causes.go` merges `jev.Questions()` and `jev.KindQuestions()`, whose ids never
collide, and code reads the fix-kind answers only for a site whose flagged
causes include allocation or string building, exactly as before. TypeSafe's
docs state that an answer does not depend on the other questions in the same
request, and a 20-function live replay against the real gateway
(`jev-batch-replay.md`) measured a mean |Δp| of 0.0107 and no changed fix-kind
pick between asking the two sets separately and asking them together, so only
the number of requests changed. When a kind leads
its cause's next kind by at least `KindGate` (0.25 sd) at a z of at least 0,
the target carries `fix_kind` and that kind's narrower remedy. Hot paths keep discovery's `path:line` verbatim, which the model
analyst used to reformat. Per-function scores are persisted with a
`cause_analysis` event, Jev's token usage is recorded under the analyst role,
and the report header names the analyst. None of it reaches `apply_policy`.

With causes ranked, code rather than a model chooses what each candidate
attacks (ADR 0013). The analysis carries its flags as structured `targets`
(function, location, cause, remedy) in the order above, and `merge_analysis`
picks the first one no earlier candidate tried (`planTarget`), writes its remedy
as the coordinator's experiment, and cuts the source excerpts down to that
function; the optimizer's instruction forbids patching any other. With a
target, the optimizer's instruction also tells it to answer with
`function_source` (the target function's whole new declaration) and
`imports` instead of a hand-written `patch` (ADR 0022): deterministic code
finds the function by name in the base revision and builds the diff itself,
which removes context-line and header mismatches as a way to lose a
candidate. `patch` stays the transport when there is no target, and remains
an accepted fallback even with one. The
optimizer's input is then an `OptimizerBrief` (the optimization mode, the
target, its excerpts and the prior candidates) rather than the whole campaign
state: discovery evidence, the full analysis and the repository inventory are
things it has been told not to act on. The session keeps the full state for the
deterministic nodes, and a cycle with no target left hands the optimizer
everything, as before. Tried targets
are recorded with each verdict and handed back on resume, so no measured
target is attacked twice (an unmeasured one gets one retry, see step 3 of the
evaluation), and once every flagged target has been tried the optimizer
chooses freely again. The coordinator model is not called in this mode, because
nothing is left for it to decide. On a live gron campaign it took up to 2m40s a
cycle, and left with a ranked list the optimizer ignored the top target and
micro-optimized `validIdentifier` (inconclusive, -0.85%) before taking the top
target on its second attempt: the bufio writer around the output loop, accepted
at -15.1% wall time. A model analyst ranks nothing, so none of this changes its
path.

**Multi-function targets (ADR 0027, proposed, `internal/campaign/callers.go`).** Every
target above is confined to one function. A separate, code-only signal —
never asked of Jev, never in its question set or baseline — can instead pick
a *set*: for a profiled hot function that always returns a fresh allocation
(every `return` is `&T{...}`, `new(T)`, or a same-package call checked one
level deep), it finds every call to it within the same package directory and
classifies each site as discarded, consumed (used only as the `X` of a
selector, a field read or a cheap method call, never stored or returned), or
escaping. `throwaway_result` fires when the callee allocates fresh and at
least half its call sites, two or more, are consumed or discarded — the dasel
case this was built against: `(*Value).UnpackKinds` ends every call with
`NewValue(res)`, and about a dozen of its own package's callers use the
result only to read a `Kind`. A firing signal produces a target in the first
tier, ahead of every Jev-ranked one, carrying the callee plus its ranked
consuming callers (capped at twelve total): `Kind` is
`agents.TargetFunctionSet` and the callers are `Target.Callers`. Callers are
ranked by discovery's profile weight first, then call-site count, then name
(`consumingCallers`, `internal/campaign/callers.go`) — call-site count alone
is a weak key once most callers tie at one site, as dasel's do, so the cap
used to keep whichever caller happened to sort first rather than whichever
one mattered. `Engine` builds `DiscoveryHotFunctionWeights` (function name to
profile hotness, uncapped by the fifteen-function hot list) alongside
`DiscoveryHotFunctions` at every point discovery profiles, threaded through
`orchestrator.DiscoveryEvidence` to the cause analyst; a caller absent from
the profile ranks after every profiled one, and an empty or nil weight map
(a benchmark-only or otherwise empty discovery) falls back to the pre-ranking
call-site-count order exactly. Every branch
that differs by kind asks `Target.IsFunctionSet()`. The optimizer answers with `function_sources` (several whole
declarations, ADR 0022's decode leniency applied to the plural field too),
and `internal/campaign/multi_function_source.go` builds one multi-file diff:
a declaration matching a name in the set replaces that function wherever it
is declared; one matching nothing in the set, and nowhere else in the
package, is a new function appended after the callee in its own file; one
matching a name that exists elsewhere in the package but outside the set is
rejected. The shape check's `checkMultiConfined` applies the same
per-function confinement across every file in the callee's directory rather
than one file. `imports` still comes back as one shared list, but each
touched file gets only the paths its own new code references
(`importsForFile`): a package name is read from the file's unresolved
selector bases, the same reading `dropOrphanedImports` and the shape check's
`checkImports` already use, so a rewritten caller in a file of its own gets
the import it needs instead of only the callee's file getting it. A name the
optimizer never listed but that resolves to an unambiguous standard-library
package (`stdlibImportPath`, the same fixed set `checkImports` treats as safe
to infer) is added the same way; nothing outside that table is ever guessed.
The single-function path (the zero `TargetFunction` kind) is unchanged, byte
for byte.

`AI_GATEWAY_API_KEY` (and optionally `AI_GATEWAY_BASE_URL`) configure the
client. `--adk` spends one preflight request before repository work, because a
gateway account without a card on file refuses every request and the analyst
node is reached only after the baseline is built. Calls are sequential and
retry HTTP 429 and 5xx with 1-16 s backoff: the provider throttled 36% of
attempts at twelve in flight, and even sequential calls met a 429 that
outlasted a 15 s ladder. `--adk-stub --analyst jev` uses a no-network stub,
and a resumed campaign that started with `--analyst jev` must be given it
again, the same rule as `--adk`.

Jev only classifies what discovery lists. On gron the output loop behind the
accepted `bufio` patch appears in discovery only as `fmt.(*pp).doPrintln`, a
standard-library frame that is dropped rather than attributed to its caller,
so the analyst is never asked about it; asked directly, it flags unbuffered
I/O at +3.3 sd.

## Jev reviewer

`--reviewer jev` (with `--adk` or `--adk-stub`) replaces the reviewer model role
the same way (ADR 0014): `Dependencies.Review` swaps in a function node named
`reviewer` that calls `internal/campaign/review.go`, which asks Jev one yes/no
question per behaviour hazard about the patch: output order that depends on map
iteration or scheduling, changed errors or exit status, a dropped error from a
call that can fail, an effect skipped on some path, reused-buffer aliasing, new
concurrency, changed number formatting, and a diff wider than its hypothesis.
The state is the hypothesis, the diff, and the patched function's source at the
base revision, found from the diff's first hunk; nothing else.

A hazard is raised when Jev answers yes (probability at least one half) and the
answer is at least two standard deviations above its usual answer to that
question on 93 real, merged performance patches (`internal/jev/review_baseline.go`,
digest-guarded like the cause baseline). On the reviewer benchmark that caught
every injected hazard whose label was right, and raised a concern on 14% of the
real patches, most of them genuine: GitHub bufio fixes that discard the flush
error, and both gron patches gotorque itself accepted, which `defer` the flush.
With the measured baseline a yes on any question is already that unusual, so
today the floor decides and z orders the concerns; a test fails if a new
baseline breaks that.

Until this, the reviewer's answer reached a policy input the policy ignores and
nothing kept it. Its concerns are now recorded with the verdict, printed in the
report under the candidate, and carried into the next cycle's
`prior_candidates`, whichever reviewer ran. They remain advice: the policy
never reads them.

## Jev explorer

`--explorer jev` (with `--adk` or `--adk-stub`) replaces the explorer model role
(ADR 0015). The model's proposals were validated by `run_discovery` and counted,
but never run, so discovery only ever sampled the manifest's first seed and a
CLI's other modes were invisible to it: gron's `--stream` runs `gronStream`,
which the seed never reaches. With the flag, the variants are chosen before
discovery samples anything (`internal/campaign/explore.go`), and the explorer
node answers at once with that plan (`agents.PlannedExplorer`):

1. Code lists the boolean options the target declares
   (`internal/workload.BoolFlags`): standard `flag` and pflag/cobra `Bool`,
   `BoolVar`, `BoolP` and `BoolVarP` calls on any receiver, and go-flags
   `long:` tags on bool fields. Spellings bound to one variable are one option
   (gron's `-s` and `--stream`). They are read from the build package or, when
   it declares none (gojq's live in `./cli`), from the module package that
   declares the most. Options the seed already passes are skipped.
2. Code runs the target's `--help` in the sandbox and keeps what it printed on
   either stream, whatever the exit status.
3. Jev answers, in one request whose state is the command and its help text,
   one yes/no question per option: does it change how the program processes
   its input or formats its output. An option is a processing mode at
   probability one half or more (`jev.ModeFloor`); each option is judged on its
   own, so no baseline is needed. On gron and gojq the modes answered 0.89 to
   0.98, while `--help`, `--version`, `--insecure` and `--exit-status` answered
   0.09 to 0.15, and gojq's `--from-file` 0.49. An earlier wording that asked
   whether a typical user passes the option put nearly everything below one
   half.
4. Code runs each mode once on the seed input, likeliest first, with the option
   placed before the seed's arguments (the standard flag package stops at the
   first positional argument), and keeps those that exit 0 with output
   different from the seed's: an option that fails on this input or changes
   nothing reaches no new code. At most three are kept
   (`maxExploredWorkloads`), since each costs a sampling window.
5. Discovery samples each kept variant after the seed, on the seed's amplified
   input and, if the target exits before the sampler attaches, on the seed
   input repeated one copy per line. gron's `--stream` reads one document per
   line of at most 1 MiB and exits at once on the 16 MiB single-line document,
   while its default mode would finish the line-repeated input in 10 ms, so
   neither shape serves both. A variant that cannot be sampled either way is
   recorded (`workload_sample_skipped`) and left out.
6. The samples are merged by share (`mergeAttributed`): each sample's
   attributed weights become fractions of that sample before they are summed,
   so a mode only one variant reaches ranks by its share of that variant's time
   instead of disappearing behind the seed's.

On gron the variants were `--stream` (0.97), `--json` (0.95) and `--no-sort`
(0.93). `gronStream` entered the hot list third and the `--json` path
(`jsonify`, `statementsFromJSON`) entered it too, while `strconv.Atoi` and
`strings.Join`, which had filled its tail, dropped out.

Jev decides nothing here. Code finds the options, runs them, and decides which
reach the profile, and Jev only orders and filters which ones are worth a run.
The variants only widen discovery: they are never measured workloads, and no
verdict reads them. The chosen variants are kept in campaign state
(`discovery_workloads`), in the discovery step's metadata
(`explored_workloads`), and in the report header.

## Jev answer cache

The graph loops (`route_campaign` back to `coordinator` -> `explorer` ->
`run_discovery` -> `analyst`) until a candidate is accepted or a bound is hit,
and at the same base revision every one of those cycles asks the analyst about
the same hot functions and the explorer about the same command and `--help`
text: recorded campaigns made 64 analyst requests for 4 optimizer calls on
go-jsonnet and 42 for 3 on dasel, almost all of them a rerun of the previous
cycle's question. `internal/campaign/jevcache.go` (ADR 0028) memoizes a Jev
response by `jev.Digest(req)` — the exact model id, state, and question set a
request carries, the same three things `Evaluate` sends over the wire — so a
repeated request is answered without a round trip. `noteAnalyst`
(`engine.go`, called from both `attachADK` on a fresh campaign and `SetADK` on
resume) wraps `roleSet.CauseEvaluator` and `roleSet.ExploreEvaluator` in a
`cachingEvaluator` before either reaches the graph; the reviewer is not
wrapped, because its state carries the candidate's own patch text, which
differs by construction from one candidate to the next, so it was checked and
found not to repeat.

The cache lives in the campaign's bbolt store (`Store.JevCacheGet`/`JevCachePut`,
bucket `jev_cache`), not in engine memory, so `--resume` reuses it: anything
that must survive resume goes in bbolt, because in-graph `CampaignState` is
rebuilt on every entry. A failed or partial response is never cached — a
cached error would deny every later request the retry that might have
succeeded — and a request whose state cannot be marshaled (never expected to
happen with `jev.Request`'s field types) is asked directly rather than
crashing the campaign over what is only ever an optimization. Hit and miss
counts are kept per role in `State.JevCache` (`JevCacheSnapshot`), updated
in-process and persisted the same way `TokenUsage` is: they ride along on the
next `saveEvent`, so a killed process still leaves its counts in bbolt. The
report's "Jev cache" table (`writeJevCache` in `report.go`) reads them back,
mirroring the "Model usage" table's shape.

## Discovery benchmark profiling

Before the model phase, the engine runs one best-effort profiling pass
(`collectDiscoveryProfile`), which samples the built binary on a manifest seed
workload first and falls back to Go benchmark CPU profiles, so every target
gets hot-path evidence. Sampling comes first because the measured workloads
are what the campaign is about: a benchmark CPU profile weights every
benchmark in the module equally regardless of how much it resembles the
command, so microbenchmarks dominate the hot list and point the optimizer at
code that cannot move the measured wall time. On gron three identifier
microbenchmarks put `validFirstRune` at 36% cumulative while the measured
workload's own hot frames (`write`, `statements.Less`, `statement.String`)
never appeared, and the first two candidates of every campaign attacked rune
classification before reaching the real cost. The completed event names the
source it used (`measured N hot functions from a target sample`, `… from
target benchmarks`, or `… from no source`).

`sampleTargetProfile` runs the first representative seed workload against the
release baseline binary under the platform sampler (macOS `sample`, Linux
`perf`), records the hottest frames as discovery evidence, and preserves the
raw report under `profile-sample/` in the campaign directory. It needs a
target that outlives the sampling window, so the workload input is amplified
first: `amplifyStdin` replicates the elements of the largest JSON array,
which keeps the document valid and multiplies the work it describes, and
falls back to repeating raw bytes for input that is not JSON. Repeating bytes
alone only lengthens the run for a target that consumes all of stdin. A
single-shot JSON CLI reads one document and ignores the rest, so gron
finished a 16 MiB concatenation of its 84 KiB seed in 21 ms, exactly as fast
as the unamplified seed, and the sampler could never attach. Safer frames are
annotated with source positions through the same repository search the
benchmark path uses, because sampler frames name a symbol but no position.

The sampled hot list ranks the target's own functions by the samples spent on
their behalf, not by self time. The sampler's top-of-stack section says which
frames were executing but not for whom, and a Go CLI that spends its time
printing is executing `fmt` and `write`: gron's per-statement `Fprintln` loop,
whose `bufio` fix was the only patch ever accepted on it, had almost no self
time and never appeared, so no analyst was ever asked about it.
`internal/profile/stacks.go` rebuilds weighted call paths from the report's
call graph (each node's own weight is its inclusive count minus its
children's; `perf script` already lists whole stacks) and credits every sample
to the innermost frame whose package is `main` or one of the module's
(`AttributeToOwn`), so a function carries the library and system calls it
makes. One gap needs an estimate: a Go system call switches to the system stack
through `runtime.asmcgocall`, and macOS `sample` cannot unwind back across it,
so on gron 141 samples spent in `write` sat under `asmcgocall` with no Go frame
above them, while only three were caught with the whole path from the output
loop down to `syscall.write`. Such orphaned samples are shared among the own
functions observed calling the matching Go wrapper (`syscall.write` for
`write`), in proportion to how often each was seen; a call with no observed
caller, such as a thread parked in `__psynch_cvwait`, stays unattributed. On
gron the output loop moved from absent to second of fifteen, behind the sort
comparator, which now also carries the `strconv.Atoi` calls its natural sort
makes. The top-of-stack names still fill any budget attribution leaves, so a
report whose call paths do not parse lists what it listed before.

When sampling succeeds the benchmark profile is still collected if the module
declares benchmarks, because the informational PGO lane is built from it. That
lane never changes a verdict, so it is bounded rather than trusted: each
profile-guided build gets five minutes, and the lane is skipped outright when
the campaign has less than three such budgets left to spend. On gron one
`-pgo` build ran for thirty-six minutes inside a forty-minute campaign, the
deadline fired mid-lane, and a candidate whose measurement had already
completed was never recorded — the campaign ended with no verdict at all.
`profileHotFunctions` executes `go test -bench . -cpuprofile` against a
single package at a time, because the go command rejects `-cpuprofile` for
more than one package and `./...` is therefore never a usable profiling
target. `benchmarkPackageOrder` tries the manifest's target package first and
then every benchmark-bearing package in the module, richest first:
`internal/profile.BenchmarkPackages` counts `func Benchmark…` declarations in
`_test.go` files, breaking ties lexicographically so repeated campaigns
profile the same package. A CLI's command package typically declares no
benchmarks while the library packages it drives do (gojq benchmarks its
evaluator, not `./cmd/gojq`), and the widened attempt still gives those
targets benchmark evidence when sampling is unavailable.

The profile is summarized through `go tool pprof`. Summarizing scans four
times the hot-function budget (`hotFunctionScanDepth`) to fill it, because a
Go CPU profile's hottest nodes are overwhelmingly runtime scheduler and
allocator frames; scanning only as deep as the budget yields a handful of
module functions and spends the rest on frames no patch can touch.
`hotFunctionNames` then deduplicates and drops `runtime.` frames, and
`actionableSymbol` drops the rest of what no source change can address:
unqualified and `_`-prefixed symbols, which is how the OS sampler's kernel and
libc names (`__psynch_cvwait`, `kevent`, `nanosleep`) present and which
describe a process waiting rather than computing. Go symbols always carry a
package qualifier, so a missing dot is a reliable discriminator; `testing.`
harness frames; and the module's own `Benchmark`, `Test`, `Fuzz` and `Example`
entry points, which are measurement scaffolding rather than the program the
target ships. Agents otherwise rank that scaffolding as a top hot path and
reason about it as target code.

Up to 15 surviving names (`hotFunctionBudget`) are annotated with source
positions: `go tool pprof -list` over the same profile where it resolves,
otherwise a repository search for the declaration, which matches through
closure suffixes (`outer.func1`, `outer.func1.2`) and method receivers
(`pkg.(*T).method`) that no `func` declaration is ever written with.
`go tool pprof -list` reports absolute paths, and the excerpt collector
refuses those because an absolute location is indistinguishable from one
escaping the repository, so every profiled frame resolved to a location no
source window could be read from. Positions are rewritten
repository-relative, and frames in the standard library or module cache are
dropped outright rather than kept as bare paths, since no patch this campaign
may write can reach them and they would otherwise occupy the excerpt budget.
Unresolvable functions keep their bare names so no entry is lost. Names that
resolve to a location already listed are folded into it: a value method and
the pointer wrapper Go generates for it are two symbols with one declaration,
and gron's list named `statements.go:312` twice. Resolution continues through
further candidates until the budget holds fifteen distinct entries.

The annotated locations are stored in campaign state as
`discovery_hot_functions` along with the raw summary artifact, and surface to
the analyst and coordinator as measured hot functions. Only when both sources
fail does the engine record a `discovery_profile_skipped` event and leave
discovery evidence empty, rather than failing the campaign.

When the campaign's resolved objective is `peak_memory_bytes` and one of the
two sources above succeeded, `Engine.profileAllocations` (ADR 0024) runs the
same benchmark packages a second time, this time under `-memprofile`
(`toolchain.TestRequest.Memprofile`, plumbed through `testArgs` exactly like
`Cpuprofile`, including the `-o` fix that keeps the compiled test binary out
of the canonical checkout). The heap profile is summarized by its
`alloc_space` sample index — cumulative bytes ever allocated, not bytes still
live at profile time — through a dedicated `Toolchain.PprofTopAllocSpace` /
`Collector.SummarizePprofAllocSpace` pair, since `go tool pprof -top` defaults
to `inuse_space`. The resulting functions are annotated with source positions
the same way as the CPU profile's, then merged to the front of
`discovery_hot_functions` (`mergeAllocFirst`): every allocation-heavy
location first, in its own rank order, then whatever CPU-derived locations
are not already present, deduplicated and capped at the same
`hotFunctionBudget`. A module with no benchmarks is skipped silently beyond a
`discovery_alloc_profile_skipped` event — discovery still has its CPU
evidence, exactly as it did before this objective existed. Either way,
`State.DiscoveryProfileSource` records which profile(s) chose the hot list
("a target sample", "target benchmarks", or either plus "a benchmark
alloc_space profile"), and the report's header states it.

## Run modes

- `discovery`: coverage-instrumented execution used to find reachable paths.
- `diagnosis`: pprof or execution-trace collection used to explain cost.
- `measurement`: release-equivalent execution used for authoritative timing.
- `validation`: tests, race detection, fuzzing, differential output, and
  invariants.

Instrumentation results must not be mixed with authoritative performance
measurements. Interleaved A/B candidate measurements always use measurement
mode against non-instrumented release builds.

## Model-boundary hardening

Model responses cross a leniency layer before any workflow node parses them
as typed results. The layer exists because hosted models return judgment
output in several shapes, and brittle rejections waste whole campaign turns.
Deterministic policy still validates everything downstream, so tolerance here
only removes parse failures of otherwise usable recommendations.

- Fence and prose extraction (`internal/agents/fence.go`): a single
  Markdown code fence with an optional language tag is stripped, and the
  first balanced JSON object or array is extracted from surrounding prose.
  Text with no extractable payload passes through unchanged. Only complete
  responses are rewritten; streaming partials pass through untouched.
- Common-malformation repair (`internal/agents/decode.go`): trailing
  commas before object or array closers are removed, string contents left
  untouched. If parsing still fails, unescaped double quotes embedded inside
  JSON string values are escaped heuristically and repair is retried once.
- Tolerant field shapes: fields declared as string arrays also accept a
  single string, an object collapsed to its most identifying scalar field,
  or an array of objects likewise collapsed. Booleans accept common string
  spellings. Hot-path lists accept objects, strings, or grouped objects.
- Patch transport (`internal/agents/types.go`): the optimizer is
  instructed to carry its unified diff as a JSON array of lines, and
  `flexPatch` joins one back into a diff. A diff embedded in a single JSON
  string is the shape models escape worst, and one bad escape corrupts the
  whole patch; an array confines the damage to the line containing it. The
  string form stays accepted, both because proposals persisted by earlier
  runs replay through this decoder when a campaign directory resumes, and
  because a model that ignores the instruction still produces a candidate the
  deterministic gates can judge. Wrapper objects such as `{"content": "…"}`
  collapse through the shared text extraction.
- Reasoning-only turns (`fence.go`): a response carrying no answer part is
  converted into an error rather than yielded. ADK's two consumers of such a
  turn disagree, and the disagreement is fatal: the workflow agent node stamps
  the turn's empty text as the node's output, while the LLM flow classifies it
  as thinking rather than answering and calls the model again inside the same
  node execution. The second call's answer becomes a second output-bearing
  event and the scheduler kills the run with `ErrMultipleOutputs`. An error
  costs one node instead of the campaign. A reasoning model that spends its
  whole output budget thinking produces exactly this turn. An answer that
  merely failed to parse as JSON is still yielded, because downstream decoding
  names the offending text. At most one complete response escapes a call, by
  construction rather than by trusting the inner iterator.
- Repair recording: when a payload parses only after a repair that can
  change its meaning (escaped control characters or quotes, completed
  closers, a dropped stray quote, a string closed where the output was cut
  off), `DecodeResultWithRepair` names the repair. Fence unwrapping and
  trailing-comma removal are not counted. Workflow nodes record a
  `role_repaired` event, and the optimizer's repair lands on its candidate
  record as `proposal_repair` ("Proposal salvaged" in the report). Before
  this, a patch cut off at the output-token cap was closed by the decoder,
  had its hunk counts recomputed by normalization, and read in every record
  like one the model meant. The repaired value is still judged normally, and
  policy never reads the field.
- Retry and usage decoration: the OpenAI-compatible provider wraps every
  role model in a decorator that transparently retries up to three attempts
  of at most six minutes each, with 15 then 30 second backoff, while a call
  fails before producing
  any content (shared-pool rate limits otherwise abort multi-hour campaigns),
  and records per-role token usage into a collector persisted with campaign
  state. Endpoint credentials and API keys are never persisted. HTTP 400,
  401, 402, 403, 404 and 422 end the ladder on the first attempt: they
  describe the request, the credential or the account, so a revoked key or
  an OpenRouter balance too low for the request (402 Payment Required) used
  to spend the whole ladder on every role before degrading anyway. They are
  recognized with `errors.As` against openai-go's `*openai.Error`, which ADK
  yields raw on the streaming path. 408, 409, 429, 5xx, transport errors, stalls and
  incomplete streams still retry. The per-attempt deadline is a
  `context.WithTimeoutCause` that names its budget, and `stream.go` reports
  `context.Cause`, so a timed-out attempt no longer reads as a bare
  `context deadline exceeded` indistinguishable from Ctrl-C or `max_duration`.
- Event-stream filtering (`internal/agents/sse.go`, `transport.go`): every
  model call reads its `text/event-stream` body through a filter below
  openai-go. openai-go dispatches an event on the blank line that ends an SSE
  comment, such as OpenRouter's documented `: OPENROUTER PROCESSING`
  keepalive, then fails the call with `unexpected end of JSON input` while
  parsing that event's empty data. It also treats a connection that closes
  cleanly before `response.completed` as a finished stream, and ADK ignores
  `response.incomplete`, so a cut or length-truncated answer used to arrive as
  a normal response with finish reason Unspecified. The filter drops events
  that carry no data, which also keeps keepalives from counting as activity
  against the idle bound. It raises `ErrStreamIncomplete` when the stream ends
  without a terminal event, or with `response.incomplete` naming its reason.
  That error carries no HTTP status, so it stays retryable. Error statuses
  pass through unfiltered so the SDK still builds the status error the ladder
  classifies.
- Per-attempt call logging (`internal/agents/observer.go`): a `CallObserver`
  receives one `CallInfo` per attempt (role, attempt number, duration, error,
  and whether a retry follows), and `--adk` wires `LogCalls` to the command's
  output as `[model_call]` lines. Role calls are the slowest and least
  observable part of a campaign; without them a run prints nothing between
  starting the workflow and the first role that completes, so a slow call, a
  silent retry, and a hung endpoint are indistinguishable until the agent
  deadline expires. Diagnosing that difference otherwise costs one campaign
  per guess.

## Model routing

Each ADK role receives its model through an injected OpenAI-compatible
provider. Per-role model IDs come from environment variables:
`GOTORQUE_MODEL_COORDINATOR`, `GOTORQUE_MODEL_EXPLORER`,
`GOTORQUE_MODEL_ANALYST`, `GOTORQUE_MODEL_OPTIMIZER`,
`GOTORQUE_MODEL_REVIEWER`. Unset roles fall back to the built-in defaults
(`deepseek/deepseek-v4.1-flash` for every role). Per-role overrides remain a
cost/latency lever: a cheap model can handle high-volume evidence work while a
stronger one handles synthesis. All builds, measurements, behavior checks,
and acceptance transitions remain deterministic and model-independent.

Before expensive repository work starts, the provider validates connectivity:
it requires `OPENROUTER_API_KEY`, checks endpoint reachability via
`OPENROUTER_BASE_URL` (defaulting to `https://openrouter.ai/api/v1`), and
verifies every configured model ID is advertised by the endpoint. It also
rejects a routed model whose advertised `top_provider.max_completion_tokens`
is below `MaxOutputTokens` (32768, what every role requests), naming the role.
Such a model would otherwise fail every call with a client error at request
time. A model that does not advertise the field passes.

Reasoning effort is optional per role:
`GOTORQUE_REASONING_{COORDINATOR,EXPLORER,ANALYST,OPTIMIZER,REVIEWER}` takes
`low`, `medium` or `high`, and anything else fails the preflight. ADK's
openaimodel maps only `MaxOutputTokens` onto the Responses API request, never
`ThinkingConfig`, so a per-role transport (`reasoningTransport`) sets
`reasoning.effort` on the request body. Unset sends nothing and leaves the
provider's default, with one exception: under `--analyst jev` code chooses the
target and remedy, the optimizer only writes one small diff, and its effort
defaults to `low`. At the provider's default a live gojq campaign ran past the
32k completion budget on all four attempts of a cycle and the breaker ended
it; at `low` every call answered in two to three minutes. An explicit
`GOTORQUE_REASONING_OPTIMIZER` still wins. Campaign state does not record the routed model IDs or
efforts.

Model calls and that preflight resolve their base URL through the same
`endpoint()` accessor, so they cannot disagree. Passing an empty `BaseURL` to
the OpenAI SDK silently targets `api.openai.com`, which sends the OpenRouter
key to the wrong provider and fails with HTTP 401 only after a preflight that
already passed against OpenRouter.

One model call is bounded by silence, not by total duration. ADK issues these
non-streaming (it streams only in SSE mode), so the provider decorates every
call with `internal/agents/stream.go`: it asks the endpoint to stream anyway,
rebuilds the single response a non-streaming call would have produced, and
fails the call when no chunk arrives for two minutes. The distinction is the
whole point. A whole-request timeout cannot tell a slow model from a dead
connection, and the four-minute one that used to bound these calls cut
generations that were still working: every campaign round logged two to eight
`error reading response body: context deadline exceeded` failures while
legitimate calls ran up to three minutes, and each stalled attempt consumed
its entire slot in the retry ladder. Streaming makes idleness measurable, so a
quiet call is abandoned in two minutes and retried while a producing one keeps
its time. Each attempt of the retry ladder is still capped, at six minutes,
since the optimizer, even at low effort, writes 10-12k completion tokens per
call: completed calls took up to 3m43s, and a four-minute cap cut a whole gojq
cycle's attempts while they were still producing. The client therefore
carries no total timeout at all; a header
timeout still fails a dead endpoint before any byte arrives.

Rebuilding the response is not a pass-through. ADK's streaming path yields the
answer as deltas and then, as its final item, an aggregated response that
repeats the answer with any reasoning text beside it; taking that last item
verbatim produced `{"ok":true}{"ok":true}` on a live OpenRouter call and mixed
deliberation into the payload the decoder parses. The decorator accumulates the
non-thought deltas instead and holds the last item back until another arrives,
so the answer is delivered once; usage metadata still comes from that last item,
which is where the endpoint reports it.

Per-role token accounting survives the change: verified against the live
endpoint, one call yields exactly one response with its usage attached.

The attempt deadline in `fence.go` (four minutes per ladder attempt) remains as
the backstop, sized so attempts×timeout + backoff fits the orchestrator's
twenty-minute per-node agent deadline.

`--analyst jev` takes the analyst off this path entirely: it calls the Vercel
AI Gateway with `AI_GATEWAY_API_KEY` instead of OpenRouter (see Jev cause
analyst). The analyst's routed model is still validated and built, but never
called.

Role output shape is not enforced by the endpoint. Each role's instruction
states strict JSON rules, and `internal/agents/decode.go` repairs the defects
models actually emit (fenced blocks, unterminated strings, missing brackets).
`roleResponseSchema` can derive a structured-output schema per role, but
requesting one restricts OpenRouter to providers advertising
`structured_outputs`, which for the default model is a single saturated
provider; see the comment on that function before enabling it.

## Campaign bounds

`max_duration` names a campaign, not a process. `withCampaignDeadline` spends
what is left of the budget rather than granting it again, because an
interrupted campaign resuming with a full budget could otherwise run for
arbitrarily many multiples of `max_duration` across enough resumes. The
remaining budget is `max_duration` minus the persisted `ElapsedRunTime`, the
wall time summed over every process that has worked on the campaign;
`StartedAt` cannot stand in for it, since a campaign is idle between an
interruption and its resume and charging that idle time would expire any
campaign resumed the next day. An exhausted budget yields an already-expired
context, so a campaign with nothing left stops down the same path as one that
runs out mid-flight.

Two clocks have to agree. Go timers run on the monotonic clock, which stops
while the machine is suspended, so a laptop that sleeps two hours mid-campaign
hands a 90-minute context two extra hours of wall time, while `ElapsedRunTime`,
measured with `time.Now`, keeps counting across the suspend and charges every
one of those minutes against the same budget. `guardWallClockBudget` polls the
wall clock every 250 ms against a fixed deadline and cancels the context when
it passes, so whichever clock runs out first ends the run. The goroutine owns
no engine state beyond the clock and exits with the context.

`stop_after_failures` bounds a run of rejected candidates. By default an
inconclusive verdict — one whose measurement did not resolve either way —
counts toward the same streak, which is why every campaign in this
repository stopped at four attempts of its twelve. A manifest can set
`stop_after_inconclusive` to give unresolved verdicts their own streak
instead; the campaign then stops on whichever of the two consecutive bounds it
reaches first, and the stop reason names the bound rather than the pair. Both
streaks are persisted (`consecutive_failures`, `consecutive_inconclusive`) and
carried into the next process the same way, because the graph rebuilds its
`CampaignState` on every entry.

`DeterministicTimeout` (`internal/orchestrator/config.go`) is a separate,
per-node bound. The ADK scheduler wraps every deterministic node in
`context.WithTimeout`, and that includes `evaluate_candidate`: build, test
gate, A/B measurement and the PGO lane. The `--adk` path used to set it from
the manifest's `minimum_command_timeout`, a per-command floor that is 30s in
every shipped manifest. gron evaluations fit (6–10s), but a heavier target or
a cold build cache would have hit it. A node deadline surfaces as
`DeadlineExceeded`, deterministic nodes have no degraded fallback, and the
campaign ended `interrupted` with a bare `context deadline exceeded` that the
next resume would hit again. The CI stub config used twenty minutes, so CI
never saw it. `orchestratorConfigFromManifest` (`internal/cli/root.go`) now
keeps `DefaultConfig`'s twenty minutes, nested inside `max_duration`.

A spent budget otherwise surfaces as whatever call happened to be in flight,
a git status, a model request, an ADK graph that drained without producing a
result, naming an innocent bystander instead of the bound that stopped the
campaign, so the context cause wins: the run ends as `interrupted` with
`ErrDurationBudgetExhausted` and a stop reason naming the budget and what was
spent of it.

## Resume semantics

Campaign steps are checkpointed in bbolt (`CompletedSteps`), so an
interrupted campaign resumes completed phases instead of redoing them.
In-memory agent clients cannot be serialized, so `optimize --resume DIR`
requires re-supplying the agent mode: pass `--adk` again for live agents
(the provider and role set are rebuilt from the environment) or `--adk-stub`
for deterministic stubs. Campaign state records `adk_mode`, and resuming a
campaign that was started with agents without passing either flag fails with
an explicit error. Because a resumed campaign takes its manifest from
persisted state and `--resume` rejects an explicit `--manifest`, the resume
path reads the manifest path out of state rather than requiring the flag.
Demanding one made `optimize --resume DIR --adk` impossible to satisfy in
either direction. `--resume` cannot be combined with `--repo`, `--manifest`,
or `--campaign-dir`.

The stop bounds are campaign-wide, not per-process. The ADK graph builds a
fresh `CampaignState` every time it is entered, so a tally living only there
restarts at zero on every resume and the bound holds only within one process.
`ConsecutiveFailures` is therefore persisted at every policy decision and fed
back as `PriorConsecutiveFailures` when the graph is re-entered.

Token usage is campaign-wide too. Provider usage collectors are created per
process, and `TokenUsage` used to be overwritten with the current process's
totals when `RunADK` returned. A resumed campaign reported only its last
process's spend, and a killed process's spend never reached bbolt.
`recordTokenUsage` now adds the process's cumulative collector snapshot to a
baseline captured before the process wrote anything. `saveEvent` refreshes it
on every event while an ADK run is active. Each refresh recomputes baseline
plus snapshot rather than adding a delta, so repeated calls never
double-count.

bbolt's exclusive lock is the liveness signal. A second process that tries to
open a campaign another one is running is told so, instead of receiving
bbolt's bare `timeout`. `gotorque report` falls back to the `report.json`
snapshot while the lock is held, so the campaign really is live. When the
report can open the database and the stored status still says `running`, the
owning process died without recording a stop. The report shows it as
`interrupted` with that stop reason and writes nothing back.

`MaxCandidates` is a known exception: `CandidatesTried` is not persisted, so
`max_candidate_patches` still binds per process and a campaign resumed enough
times can exceed it.

## Reports

`gotorque report DIR` renders Markdown or JSON from persisted state. The
report includes the environment and revision, repository inventory, baseline
workload results, and one section per candidate experiment with the verdict
(accepted, rejected, inconclusive), hypothesis, patch path, evidence summary,
policy reasons, a metric comparison table (baseline, candidate, delta
percent, statistical support), trimmed benchstat output when benchstat
contributed, and a per-role token usage table.

A running campaign holds the database's exclusive lock, so a report read
while one is in flight used to fail with bbolt's lock timeout. The engine
rewrites `report.json` and `report.md` after every verdict, and `report`
falls back to that snapshot when the database cannot be opened: the snapshot
is the state as of the last candidate, not the live one.

## Dependency policy

Use maintained libraries aggressively for orchestration, protocols, schemas,
profile parsing, CLI structure, and tests. Invoke `go`, `go tool`, `git`,
GNU `patch`, and `benchstat` directly when they are the authoritative
implementation. All dependencies are pinned and wrapped behind narrow package
interfaces. `benchstat` is optional; campaigns complete without it using the
internal t-test.

## Security boundary and isolation probing

Target commands run without network access and with writes confined to a
fresh temporary directory unless a target manifest's `sandbox` block
explicitly grants additional capabilities (ADR 0026). That block is
translated once per campaign into a `runner.SandboxPolicy`
(`internal/campaign/engine.go`'s `sandboxPolicy`) and set on the `Runner` at
construction, not per request: every run the campaign makes — discovery,
candidate A/B, the confirmation series, the PGO lane, explore variants, and
the profiler's sampled runs — shares one policy, so a baseline and a
candidate are never isolated differently. `sandbox.network` drives network
denial (previously this was tied only to whether local isolation was
available at all, ignoring what the manifest asked for);
`sandbox.filesystem.write` drives whether writes stay confined to the
sandbox; `sandbox.environment.allow`/`passthrough` drive which environment
variables a workload process receives, so gotorque's own credentials (model
provider API keys) never reach a target. `sandbox.max_processes` is declared
and validated but never enforced, on any platform: `RLIMIT_NPROC` counts
threads on Linux and is scoped per user account rather than per process
tree, so bounding it to the value every manifest sets (1) crashes the Go
runtime's own thread creation rather than bounding a run's process tree.
`sandbox.max_memory_bytes` is attempted via the soft `RLIMIT_AS`
(`ulimit -S -v`, failing loudly with `|| exit 125` rather than silently
running unbounded) wrapped around the command before any local-isolation
exec wrapper, on Linux only, and only for a measured or discovery run —
never while profiling, where the profiler attaches to or runs the target
directly. When nothing is enforceable the command is returned completely
unwrapped, which the macOS sampler depends on: it attaches to the target's
PID immediately after starting it, and a shell wrapper in between delays
the `exec` past that attach. See `docs/target-manifest.md`'s sandbox section
for what each field enforces per platform and what it degrades to. The
runner only launches a configured build artifact and rejects workload
command paths that differ from it. Candidate changes are isolated in Git
worktrees; accepted patches are copied to the campaign's `accepted/`
directory and are never pushed anywhere by the harness.

Separately, `internal/toolchain.Toolchain.run` — which every `go build`/
`go test`/benchstat invocation goes through, including the test gate and
measurement builds that compile and run a model-written candidate patch —
strips any environment variable shaped like a credential
(`OPENROUTER_API_KEY`, `AI_GATEWAY_API_KEY`, or any name ending in
`_API_KEY`/`_TOKEN`/`_SECRET` or containing `PASSWORD`) from gotorque's own
process environment before passing the rest through. That is a different
leak from the sandbox's `environment.allow`/`passthrough`: it is not
manifest-controlled, and it closes off candidate code compiled and executed
as part of the test gate from reading a model provider credential that was
only ever meant for gotorque's own calls to the model API.

On Linux, isolation uses bubblewrap: read-only bind of `/`, writable sandbox
root, and `--unshare-net` when network is denied. On macOS it uses
`sandbox-exec` with a generated profile restricting writes to the sandbox.
Because some environments block the operations these tools need, the runner
probes capabilities once per process:

- If bubblewrap cannot perform even the basic bind-and-chdir setup (common in
  nested CI containers lacking mount privileges), commands run unwrapped
  rather than failing every campaign.
- If bubblewrap works but cannot create a network namespace (GitHub's nested
  virtualization blocks loopback configuration even with SYS_ADMIN), the
  wrapper degrades to filesystem-only isolation without `--unshare-net`.

Authoritative measurement environments should provide working bubblewrap;
evidence gathered with degraded isolation is not equivalent.

## CI

CI runs unit tests under the race detector, builds the CLI, validates the checked-in target
manifests, and performs a deterministic stub-agent smoke campaign against a
pinned gojq clone, with no model endpoint involved. It runs in a container
granted `SYS_ADMIN` so bubblewrap can create namespaces where the host allows
it. No workflow runs a model-driven campaign; live `--adk` campaigns are run
by hand.
