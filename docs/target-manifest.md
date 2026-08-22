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
  "$schema": "https://example.com/go-agent-optimizer/target-manifest-v1.schema.json",
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
  "optimization_policy": "idiomatic"
}
```

The example omits optional performance and campaign values. The loader fills
them with the version-one defaults below.

## Workloads and hybrid discovery

Each seed has an `args` array, optional deterministic `stdin`, optional fixture
`files`, a tier, and provenance. Fixture paths must be relative and remain
inside the temporary sandbox. `target.command` is a fixed prefix or
subcommand; it is empty for a root CLI.

The three tiers have deliberately different roles:

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

See the example assets in [`targets/gojq`](../targets/gojq/) and
[`targets/scc`](../targets/scc/).
