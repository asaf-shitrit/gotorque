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
consecutive-failure limit is reached. The final acceptance transition is
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
   output is kept in the candidate record for reports.
7. **Policy.** `internal/policy` applies the fixed verdict order: behavior
   and safety failures are hard rejections; missing evidence or a primary
   metric that is not statistically supported or improves less than 3 percent
   is inconclusive; any guardrail (CPU time, peak memory, binary size)
   regressing more than 2 percent rejects. Every verdict is persisted with
   reasons and metric comparisons.

## Source-excerpt enrichment

After the analyst reports hot paths, `merge_analysis` calls
`internal/campaign/excerpts.go` (best effort, deterministic). Up to five
analyst hot-path locations of the form `path.go:line` are resolved to real
source windows: up to 120 lines starting 40 lines before the target line,
capped at 8 KiB each and 32 KiB total. Locations that are absolute, escape
the repository root, or do not resolve to readable files are skipped
silently. The excerpts travel in campaign state as `source_excerpts`, and
both coordinator and optimizer instructions direct the optimizer to anchor
diff context lines to excerpt text rather than guessing, which is what keeps
strict `git apply` viable on model-generated patches.

## Discovery benchmark profiling

Before the model phase, the engine runs one best-effort profiling pass
(`collectDiscoveryProfile`). It executes `go test -bench . -cpuprofile`
against the target package in the canonical checkout, requires benchmark
functions to actually run, and summarizes the resulting CPU profile through
`go tool pprof`. Up to 15 non-runtime function names are stored in campaign
state as `discovery_hot_functions` along with the raw pprof summary artifact;
these names surface to the analyst and coordinator as measured hot functions.
Any failure (no benchmarks, pprof failure, missing tool) records a
`discovery_profile_skipped` event and leaves discovery evidence empty instead
of failing the campaign.

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

- **Fence and prose extraction** (`internal/agents/fence.go`): a single
  Markdown code fence with an optional language tag is stripped, and the
  first balanced JSON object or array is extracted from surrounding prose.
  Text with no extractable payload passes through unchanged. Only complete
  responses are rewritten; streaming partials pass through untouched.
- **Common-malformation repair** (`internal/agents/decode.go`): trailing
  commas before object or array closers are removed, string contents left
  untouched. If parsing still fails, unescaped double quotes embedded inside
  JSON string values are escaped heuristically and repair is retried once.
- **Tolerant field shapes**: fields declared as string arrays also accept a
  single string, an object collapsed to its most identifying scalar field,
  or an array of objects likewise collapsed. Booleans accept common string
  spellings. Hot-path lists accept objects, strings, or grouped objects.
- **Retry and usage decoration**: the OpenAI-compatible provider wraps every
  role model in a decorator that transparently retries up to four attempts
  with 15, 30, then 60 second backoff while a call fails before producing
  any content (shared-pool rate limits otherwise abort multi-hour campaigns),
  and records per-role token usage into a collector persisted with campaign
  state. Endpoint credentials and API keys are never persisted.

## Model routing

Each ADK role receives its model through an injected OpenAI-compatible
provider. Per-role model IDs come from environment variables:
`GOTORQUE_MODEL_COORDINATOR`, `GOTORQUE_MODEL_EXPLORER`,
`GOTORQUE_MODEL_ANALYST`, `GOTORQUE_MODEL_OPTIMIZER`,
`GOTORQUE_MODEL_REVIEWER`. Unset roles fall back to the built-in defaults
(`gpt-5.6-sol` for coordinator and optimizer, `gpt-5.6-luna` for explorer,
`gpt-5.6-terra` for analyst and reviewer). This tiering is cost/latency
configuration only: cheap models handle high-volume evidence work while
stronger ones handle synthesis. All builds, measurements, behavior checks,
and acceptance transitions remain deterministic and model-independent.

Before expensive repository work starts, the provider validates connectivity:
it requires `OPENAI_API_KEY`, checks endpoint reachability via `OPENAI_BASE_URL`
(defaulting to the official OpenAI endpoint), and verifies every configured
model ID is advertised by the endpoint. Each role also carries a structured
output schema derived from its Go result type, so the endpoint enforces
JSON shape instead of relying on prompt discipline alone.

## Resume semantics

Campaign steps are checkpointed in bbolt (`CompletedSteps`), so an
interrupted campaign resumes completed phases instead of redoing them.
In-memory agent clients cannot be serialized, so `optimize --resume DIR`
requires re-supplying the agent mode: pass `--adk` again for live agents
(the provider and role set are rebuilt from the environment) or `--adk-stub`
for deterministic stubs. Campaign state records `adk_mode`, and resuming a
campaign that was started with agents without passing either flag fails with
an explicit error. `--resume` cannot be combined with `--repo`,
`--manifest`, or `--campaign-dir`.

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
