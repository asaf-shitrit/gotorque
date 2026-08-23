# gotorque

**Behavior-preserving performance optimization for Go binaries — driven by AI agents, decided by deterministic evidence.**

`gotorque` takes a Go CLI you already have, hunts for hot paths, and lets AI agents propose small source patches that could make it faster. Nothing is taken on faith: every candidate must survive the same gauntlet —

```
propose → normalize → apply in isolated worktree → build
        → upstream test suite → interleaved A/B measurement
        → statistical acceptance → accept or reject
```

A candidate is accepted only when the improvement is statistically supported
and no guardrail (CPU time, peak memory, binary size) regresses. The ratchet
only moves forward.

## Why

Optimizers are easy to write and dangerous to trust. `gotorque` exists to make
the dangerous part mechanical: models are good at *guessing* optimizations;
this harness makes them prove each guess on real measurements before it counts.
Completing a campaign with no accepted candidate is a successful result — the
harness's job is honest verdicts, not volume.

## Features

- **Bounded ADK campaign graph** — coordinator, explorer, analyst, optimizer,
  and reviewer roles over any OpenAI-compatible endpoint (OpenRouter works out
  of the box), with tiered routing: strong models for judgment, cheap ones for
  high-volume evidence work.
- **Real candidate loop** — patches are normalized (model diffs are repaired),
  fuzz-applied in isolated Git worktrees, built release-equivalently, and
  gated on the project's own test suite.
- **Honest measurement** — interleaved A/B runs against the baseline binary,
  order-insensitive output comparison for CLIs with nondeterministic tie
  ordering, Welch-style statistical support, guardrail regression checks.
- **Deterministic policy** — agents advise; code decides. Every verdict is
  persisted with its reasons and metric comparisons.
- **Campaign persistence** — bbolt-backed state, resumable mid-run
  (`optimize --resume DIR --adk`), content-addressed artifacts, Markdown/JSON
  reports that explain every decision.
- **Sandboxed by default** — network disabled, writes restricted to
  campaign-owned directories, local isolation via `sandbox-exec`/bwrap.

## Quick start

Requirements: Go 1.26+, Git, GNU patch, and optionally
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).

```sh
git clone https://github.com/asaf-shitrit/gotorque
cd gotorque
go build -o /tmp/gotorque ./cmd/gotorque

# validate an example target manifest
/tmp/gotorque manifest validate targets/gojq/manifest.json

# run a full model-driven campaign (needs an OpenAI-compatible endpoint)
export OPENAI_API_KEY=sk-or-v1-...
export OPENAI_BASE_URL=https://openrouter.ai/api/v1
export GOTORQUE_MODEL_COORDINATOR=stealth/ox-alpha
export GOTORQUE_MODEL_EXPLORER=openai/gpt-oss-120b
export GOTORQUE_MODEL_ANALYST=openai/gpt-oss-120b
export GOTORQUE_MODEL_OPTIMIZER=stealth/ox-alpha
export GOTORQUE_MODEL_REVIEWER=stealth/ox-alpha

gocache=/private/tmp/gotorque-cache /tmp/gotorque optimize \
  --repo /path/to/target-repo \
  --manifest targets/gojq/manifest.json \
  --adk

# inspect the verdicts
/tmp/gotorque report <campaign-dir>
```

No endpoint handy? `--adk-stub` runs the whole pipeline with deterministic
stub agents — useful for CI and smoke tests.

## How a campaign works

1. **Baseline** — clean-repository enforcement, release-equivalent build,
   seed workloads executed in isolated sandboxes with coverage.
2. **Discovery profiling** — benchmark CPU profiles (when available) surface
   real hot functions to the analyst.
3. **Agent cycle** — the coordinator picks one bounded experiment; the
   explorer proposes replayable workloads; the analyst interprets profiles;
   the optimizer produces one focused diff; the reviewer challenges it.
4. **The gauntlet** — deterministic code normalizes and applies the patch in
   a worktree, builds it, runs the target's tests, then measures baseline vs
   candidate in interleaved A/B pairs.
5. **Verdict** — policy accepts only statistically supported improvement
   without guardrail regressions. Reports record every attempt, decision,
   reason, and comparison.

Design boundaries: one CLI per campaign, strict behavior preservation, no
merges/pushes/PRs from the harness, macOS evidence labeled provisional and
Linux authoritative.

Isolation note: on Linux, campaigns isolate workloads through bubblewrap.
If the environment cannot support it (some nested CI containers), gotorque
detects this once and runs commands unwrapped rather than failing — use an
environment with working bubblewrap when evidence must be fully isolated.

## Targets

Example manifests live in [`targets/gojq`](targets/gojq) and
[`targets/scc`](targets/scc). A target manifest describes the repository,
build target, command shape, and seed workloads; everything else the agents
discover themselves.

## MCP server

`gotorque mcp serve --state-root PATH` exposes a typed MCP surface for
reading campaigns and driving asynchronous work from other tools.

## Development

```sh
GOCACHE=/private/tmp/gotorque-cache go test ./...
```

See [docs/architecture.md](docs/architecture.md) and the package map below
for how the pieces fit together.

| Package | Role |
|---|---|
| `internal/agents`, `internal/orchestrator` | ADK roles and bounded campaign workflow |
| `internal/campaign`, `internal/candidate` | Persistent engine, candidate evaluation loop |
| `internal/runner`, `internal/toolchain`, `internal/profile` | Sandboxing, builds, A/B measurement, pprof |
| `internal/manifest`, `internal/policy` | Target configuration, acceptance decisions |
| `internal/jobs`, `internal/mcpserver` | Async jobs, typed MCP surface |

## License

MIT — see [LICENSE](LICENSE).
