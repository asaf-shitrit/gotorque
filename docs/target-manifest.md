# Target manifest v1

A target manifest is the small, checked-in contract between a Go CLI
repository and the optimization harness. It is intentionally not a complete
benchmark suite: `workloads.seeds` provide ground-truth starting points while
`workloads.discovery` authorizes deterministic expansion from repository
inspection, help text, documentation, examples, tests, benchmarks, and seed
mutation.

Manifests are JSON and are validated by the embedded
`internal/manifest/schema/target-manifest-v1.json` using
`github.com/santhosh-tekuri/jsonschema/v6`. `manifest.Load` then applies
defaults and performs semantic checks. Unknown fields are rejected so a typo
cannot silently weaken a campaign's safety or acceptance settings.

## Minimal shape

```json
{
  "$schema": "https://example.com/gotorque/target-manifest-v1.schema.json",
  "version": "v1",
  "name": "example CLI",
  "target": {
    "repository": "https://example.com/example",
    "build": {"package": "./cmd/example", "binary": "example"},
    "command": []
  },
  "workloads": {
    "seeds": [{
      "id": "basic",
      "name": "basic invocation",
      "tier": "representative",
      "args": ["--help"],
      "provenance": "manifest"
    }],
    "discovery": {
      "enabled": true,
      "sources": ["help", "documentation", "tests", "benchmarks", "seed_mutation"],
      "strategies": ["input_size_sweep", "boundary_values", "coverage_guided"],
      "seed": 1,
      "max_cases": 100,
      "max_depth": 4
    },
    "tiers": {
      "representative": {"weight": 1.0, "acceptance_eligible": true},
      "plausible": {"weight": 0.5, "acceptance_eligible": false},
      "stress": {"weight": 0.0, "acceptance_eligible": false}
    }
  },
  "sandbox": {
    "network": "deny",
    "filesystem": {"read": "repo_and_assets", "write": "temp_only"},
    "environment": {"allow": ["LANG", "LC_ALL"], "passthrough": []},
    "max_processes": 1
  },
  "normalization": {
    "stdout": {"mode": "exact"},
    "stderr": {"mode": "exact"},
    "files": []
  },
  "performance": {},
  "campaign": {},
  "optimization_policy": "idiomatic"
}
```

Every top-level key above is required, `performance` and `campaign` included.
Their *contents* are optional, so an empty object is enough and the loader
fills in the version-one defaults below, but the objects themselves must be
present or schema validation fails with `missing properties 'performance',
'campaign'`.

Check a manifest without running a campaign:

```sh
gotorque manifest validate targets/gron/manifest.json
```

## Workloads and hybrid discovery

Each seed has an `id`, `name`, `tier`, `args` array, and `provenance`, plus
optional deterministic `stdin`, optional fixture `files`, an optional
`description`, and an optional per-seed `timeout`. Fixture paths must be relative and remain
inside the temporary sandbox. `target.command` is a fixed prefix or
subcommand; it is empty for a root CLI.

`target.build.directory` is optional and repository-relative (no `..`, no
absolute paths). It names a nested Go module the CLI lives in, for a target
that keeps `cmd/<name>` as its own module with a `replace ../../` back to the
root library — `alecthomas/chroma`'s `cmd/chroma` is one (ADR 0023). Every
build (baseline, candidate, coverage, PGO) then runs with that directory as
the working directory, and `build.package` is resolved relative to it, e.g.
`"directory": "cmd/chroma", "package": "."`. Leaving `directory` unset builds
from the repository root, exactly as every manifest did before this field
existed. The test gate still runs only the root module's `go test ./...`, so
a nested module's own tests, if it has any, are not run by gotorque; a patch
to the root library still reaches the CLI through its `replace` directive, so
the gate still exercises the code the nested module depends on. Discovery's
benchmark profiling is root-module-only for the same reason: the target
package named by `build.package` is not reachable from the repository root
when `directory` is set, so it is left out of the packages profiling tries,
and profiling falls back to the root module's own benchmarks.

The three tiers serve different roles:

- `representative`: manifest-defined or known common usage. These are the only
  workloads eligible to establish a performance acceptance.
- `plausible`: inferred from the repository and documentation. These find
  opportunities and add evidence, but cannot headline an acceptance.
- `stress`: extreme sizes, malformed inputs, and unusual shapes. They can
  expose scaling, safety, or memory regressions and veto a candidate, but
  cannot accept one alone.

The agent chooses bounded exploration goals. Deterministic generators create
the concrete mutations, size sweeps, and fixtures using the manifest seed.
Every generated workload should retain its source seed, generator strategy,
and tier in its provenance record.

## Sandbox and behavior preservation

The default target policy denies network access, allows repository and asset
reads, and restricts writes to a temporary directory. Environment variables
must be explicitly listed. `max_processes` bounds child-process fan-out.

Normalization is explicit and defaults to exact stdout/stderr and file
comparison. A target may name a specific timestamp, temporary path, identifier,
or ordering rule, but it must document that normalization. Exit status,
interface, file formats, and declared side effects remain exact behavior
constraints; normalization is not permission to change functionality.

## Performance defaults

`wall_time_ns` is the primary end-to-end metric, including process startup and
I/O. The loader defaults to a 3% minimum improvement, a 2% maximum regression
for required guardrails, and required statistical support. Default guardrails
are `peak_memory_bytes`, `cpu_time_ns`, and `binary_size_bytes`.

Campaign defaults are 90 minutes, 12 candidate patches, one candidate measured
at a time, four consecutive rejected/inconclusive candidates before stopping,
a 20-minute discovery stall timeout, a 2x baseline runtime command timeout,
and a 30-second minimum command timeout.

`campaign.stop_after_inconclusive` is optional and defaults to unset, which
keeps the historical behavior: an inconclusive verdict counts toward
`stop_after_failures`, so a run of unresolved candidates ends the campaign. Set
it to give unresolved verdicts their own bound. A campaign then stops when
consecutive *rejections* reach `stop_after_failures` or consecutive
*inconclusive* results reach `stop_after_inconclusive`, whichever comes first;
an accepted candidate clears both, and a rejection breaks the unresolved run.
This is the dial for spending the patch budget when the model keeps proposing
candidates whose measurements simply do not resolve — every campaign run in
this repository's own target set stopped at four attempts of its twelve for
that reason.

### Trade-offs per campaign

The performance block is the target's default contract. A campaign can
override it without editing the manifest: `--tradeoff` starts from a preset,
and `--allow metric=percent` (repeatable) sets any one metric's regression
limit on top of it (ADR 0020).

| Preset | Improves | May regress |
|---|---|---|
| `balanced` | the manifest's primary metric | the manifest's guardrails, unchanged |
| `speed` | `wall_time_ns` | `peak_memory_bytes` +10%, `cpu_time_ns` +5% |
| `lean` | `peak_memory_bytes` | `wall_time_ns` +3%, `cpu_time_ns` +3% |

`--allow` accepts `wall`, `cpu`, `memory`, `size` or a full metric name, and
makes that metric a required guardrail. Switching the improved metric turns the
old one into a required guardrail at `maximum_guardrail_regression_percent`
unless an allowance says otherwise. A metric cannot be improved and allowed to
regress at once. The effective limits are written into the campaign's copy of
the manifest when it is created, so a resume keeps them and refuses new
trade-off flags, and every report opens with a "Judged under" line.

Discovery still profiles CPU first, but under `lean` (or any trade-off whose
resolved objective is `peak_memory_bytes`) it also runs the module's
benchmarks, when it has any, under `-memprofile` and folds the heaviest
allocators into the hot list ahead of CPU-only functions, and the analyst
ranks each site's allocation causes ahead of its other causes (ADR 0024). A
module with no benchmarks still finds memory wins only where CPU-hot code
also happens to allocate.

The deterministic policy returns:

- `accepted` only for behavior-preserving candidates with safety checks passed,
  representative evidence, statistically supported primary improvement of at
  least 3%, and no required guardrail regression over 2%.
- `rejected` for behavior or safety failures, or statistically supported
  guardrail regressions over their configured limits.
- `inconclusive` for missing representative evidence, missing/noisy metrics,
  unsupported statistics, or improvements below the practical threshold.

The LLM agent may explain the result and choose the next bounded experiment,
but it cannot override the policy decision.

## Optimization policies

The manifest selects one Go-oriented policy:

- `idiomatic` (default): pure Go, existing dependencies only, algorithms,
  allocation and concurrency improvements, PGO, and supported compiler flags;
  no new `unsafe`, assembly, CGO, or `//go:linkname`.
- `specialized`: also permits controlled `unsafe`, generated specialized Go,
  and platform build tags, with a pure-Go fallback and extra architecture,
  fuzz, race, and alignment checks.
- `native`: also permits Go assembly, CGO, and architecture-specific code;
  requires per-architecture CI, a pure-Go fallback, and explicit portability
  and maintenance reporting.

All policies preserve behavior. The agent may create campaign-only shims,
fixtures, benchmark drivers, and instrumentation, but it may not add or
upgrade production dependencies. Version one never merges, pushes, or opens a
pull request.

## Checked-in examples

| target | repository | exercises |
| --- | --- | --- |
| [`targets/gojq`](../targets/gojq/) | `itchyny/gojq` | CPU, parsing, interpretation, allocation |
| [`targets/scc`](../targets/scc/) | `boyter/scc` | filesystem traversal, classification, scaling |
| [`targets/gron`](../targets/gron/) | `tomnomnom/gron` | JSON flattening; benchmarks live in the root package |
| [`targets/dedupe`](../targets/dedupe/) | `asaf-shitrit/gotorque-targets` | line deduplication |
| [`targets/numstats`](../targets/numstats/) | `asaf-shitrit/gotorque-targets` | numeric aggregation |

All five validate. `dedupe` and `numstats` name a repository that does not
exist yet, so they validate but cannot be run until it is published; the other
three point at live upstreams.
