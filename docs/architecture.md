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
  -> explorer (propose workload strategies)
  -> run_discovery (deterministic; validates each proposal)
  -> analyst (interpret profile and coverage evidence)
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
is executing when it expires. The final acceptance transition is
always produced by deterministic policy (`internal/policy`); agent output,
including the reviewer's recommendation, never decides acceptance by itself.

## Candidate evaluation loop

`internal/campaign/candidate_eval.go` runs the deterministic half of each
candidate cycle. The model never self-approves: every terminal judgment is
produced here or in policy.

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
   content, paths escaping the repository, edits to `go.mod`, `go.sum`,
   `default.pgo`, or vendored files, diagnostic instrumentation files, and,
   depending on the manifest's optimization policy, prohibited techniques
   such as `unsafe`, assembly, or cgo.
2. **Isolated worktree at the base revision.** A Git worktree is created from
   the recorded base revision under the campaign directory. The normalized
   patch is applied with strict `git apply --check`; when that fails because
   model context lines are approximate, GNU patch with `--fuzz=5` is tried as
   a fallback. The applied tree still faces the full test-suite gate before
   any measurement, so fuzzy application cannot smuggle in behavior changes.
   Apply errors are returned with captured stderr so the optimizer can see
   why its diff was rejected.
3. **Release build.** The patched tree is built with release-equivalent flags
   into the campaign builds directory. Build failures end the attempt with
   the compiler stderr attached to the candidate record.
4. **Upstream test-suite gate.** `go test` must pass on the patched tree
   before measurement starts. A test failure rejects the candidate without
   any timing comparison.
5. **Interleaved A/B measurement.** For each representative-tier seed
   workload, baseline and candidate binaries are measured in serialized
   alternating pairs (baseline first, seven pairs per workload) so CPU
   contention affects both sides equally. Before the pairs run, the baseline
   is executed twice against itself: if two identical runs produce different
   stdout digests, the workload is treated as nondeterministic and behavior
   comparison switches to an order-insensitive sorted-lines digest, so
   cosmetic row ordering cannot reject a behavior-preserving patch.
   Exit codes must match in all cases.
6. **Statistics.** Each metric gets a two-sample Welch t-test against a
   conservative critical value (`|t| > 2.2`, roughly p < 0.05 for these
   sample sizes); support is never reported from fewer than four samples per
   side. When the optional `benchstat` binary is installed
   (`internal/campaign/benchstat.go`), raw wall-time samples are written per
   workload and benchstat refines the result: a parseable p-value below 0.05
   grants support, a parseable but insignificant p-value withdraws support
   the coarse t-test may have granted, and delta-only legacy output is
   informational and can never grant support by itself. Trimmed benchstat
   output is kept in the candidate record for reports. Representative
   workloads are folded per repetition before the pooled comparison: the
   mean across workloads for wall and CPU time, the maximum for peak memory,
   which is a high-water mark rather than an additive quantity. Concatenating
   raw samples from workloads of different scale instead inflates the pooled
   spread with between-workload variance: on gron a real 20.8% win on the
   large workload produced a pooled `t` of 1.06 against 10.69 measured on the
   affected workload alone, and the candidate was reported inconclusive.
   Folding leaves the reported delta unchanged, because dividing every sample
   by a constant leaves a Welch t unchanged.
7. **Policy.** `internal/policy` applies the fixed verdict order: behavior
   and safety failures are hard rejections; missing evidence or a primary
   metric that is not statistically supported or improves less than 3 percent
   is inconclusive; any guardrail (CPU time, peak memory, binary size)
   regressing more than 2 percent rejects. Statistical support is required of
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
target line, capped at 8 KiB each and 32 KiB total, five excerpts at most.
Locations that are absolute, escape the repository root, repeat a
`path:line` already taken, or do not resolve to readable files are skipped
silently. The cap counts excerpts actually produced rather than locations
examined: capping candidates first meant five unusable leading locations
yielded nothing even when later ones resolved cleanly. The excerpts travel in campaign state as `source_excerpts`, and
both coordinator and optimizer instructions direct the optimizer to anchor
diff context lines to excerpt text rather than guessing, which is what keeps
strict `git apply` viable on model-generated patches.

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

When sampling succeeds the benchmark profile is still collected if the module
declares benchmarks, because the informational PGO lane is built from it.
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
Unresolvable functions keep their bare names so no entry is lost.

The annotated locations are stored in campaign state as
`discovery_hot_functions` along with the raw summary artifact, and surface to
the analyst and coordinator as measured hot functions. Only when both sources
fail does the engine record a `discovery_profile_skipped` event and leave
discovery evidence empty, rather than failing the campaign.

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
- Retry and usage decoration: the OpenAI-compatible provider wraps every
  role model in a decorator that transparently retries up to four attempts
  with 15, 30, then 60 second backoff while a call fails before producing
  any content (shared-pool rate limits otherwise abort multi-hour campaigns),
  and records per-role token usage into a collector persisted with campaign
  state. Endpoint credentials and API keys are never persisted.
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
verifies every configured model ID is advertised by the endpoint.

Model calls and that preflight resolve their base URL through the same
`endpoint()` accessor, so they cannot disagree. Passing an empty `BaseURL` to
the OpenAI SDK silently targets `api.openai.com`, which sends the OpenRouter
key to the wrong provider and fails with HTTP 401 only after a preflight that
already passed against OpenRouter.

One model call is bounded by a four-minute whole-request timeout. ADK issues
these non-streaming (it streams only in SSE mode), so the endpoint sends
nothing until generation finishes and there is no byte flow to measure
idleness against, which makes a header or idle timeout the wrong instrument.
Without any client timeout the SDK supplies a client with none, so a request
the endpoint never completes hangs until the orchestrator's per-role agent
deadline expires, consuming that role's whole budget and failing the campaign
with nothing but `context deadline exceeded`; the retry ladder cannot help,
because a stalled request never produces an error to retry. Four minutes
leaves room for the ladder's attempts inside the agent deadline, against
observed role calls of seconds to about two minutes.

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

## Dependency policy

Use maintained libraries aggressively for orchestration, protocols, schemas,
profile parsing, CLI structure, and tests. Invoke `go`, `go tool`, `git`,
GNU `patch`, and `benchstat` directly when they are the authoritative
implementation. All dependencies are pinned and wrapped behind narrow package
interfaces. `benchstat` is optional; campaigns complete without it using the
internal t-test.

## Security boundary and isolation probing

Target commands run without network access and with writes confined to a
fresh temporary directory unless a target manifest explicitly grants
additional capabilities. The runner only launches a configured build artifact
and rejects workload command paths that differ from it. Candidate changes are
isolated in Git worktrees; accepted patches are copied to the campaign's
`accepted/` directory and are never pushed anywhere by the harness.

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

CI runs unit tests, builds the CLI, validates the checked-in target
manifests, and performs a deterministic stub-agent smoke campaign against a
pinned gojq clone, with no model endpoint involved. Both CI and the nightly
workflow run in a container granted `SYS_ADMIN` so bubblewrap can create
namespaces where the host allows it. The nightly model-driven campaign is
manual-dispatch only (`workflow_dispatch`): it consumes OpenRouter tokens,
so no cron schedule is enabled. It runs gojq with live agents, uploads
campaign evidence (reports, patches, accepted diffs, logs) as artifacts, and
tolerates incomplete campaigns while reporting completion status.
