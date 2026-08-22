# Go Agent Optimizer

An AI-driven, behavior-preserving optimization harness for source-available Go
CLI repositories.

The harness uses Google ADK Go 2.x for agent and workflow orchestration. It
delegates judgment-heavy work to specialist agents while deterministic tools
build, execute, profile, compare, and validate candidate patches. The initial
validation targets are `itchyny/gojq` and `boyter/scc`.

## Status

The CLI now includes an in-process campaign engine with bbolt persistence,
resume, clean-repository enforcement, read-only local builds, isolated seed
workload execution, content-addressed evidence, Markdown/JSON reports, and a
bounded ADK graph. Pass `--adk` to `optimize` to construct the five role agents
from the OpenAI-compatible endpoint configured by environment variables.
Deterministic candidate worktree validation and statistical acceptance remain
guardrails around the model loop.

The candidate loop is fully wired: model-proposed patches are validated,
normalized, applied in isolated worktrees (strict `git apply` first, GNU patch
fuzz matching as fallback), gated on the upstream test suite, and measured with
interleaved A/B runs against the baseline binary. Policy accepts a candidate
only on statistically supported improvement without guardrail regressions;
per-attempt verdicts, reasons, and metric comparisons appear in campaign
reports. Resume supports re-attaching model agents with `--resume --adk`.
Model routing is tiered via environment variables: judgment-heavy roles
(coordinator, optimizer, reviewer) can use a stronger model while high-volume
evidence roles (explorer, analyst) use cheaper ones.

## Design boundaries

- One CLI command or subcommand per optimization campaign.
- Strict behavior preservation.
- Network disabled and writes restricted to campaign-owned temporary
  directories by default.
- Local worktrees and temporary commits only; no merge, push, or PR creation.
- macOS evidence is labeled provisional; Linux evidence is labeled
  authoritative.
- Default acceptance: at least 3% end-to-end runtime improvement with
  statistical support and no guarded metric regression above 2%.
- Existing production dependencies cannot be added or upgraded.

## Architecture

See [docs/architecture.md](docs/architecture.md) and the living design plan at
[`../go-agent-optimization-harness-plan.md`](../go-agent-optimization-harness-plan.md).

## Development

Requirements:

- Go 1.26 or newer for this repository (`ADK Go v2` itself requires Go 1.25+).
- Git.
- `benchstat` from `golang.org/x/perf/cmd/benchstat` for authoritative
  benchmark comparisons.

On a sandboxed macOS environment, keep the Go build cache in a writable
location:

```bash
GOCACHE=/private/tmp/go-agent-optimizer-cache go test ./...
```

Build the operator CLI and validate either example target:

```bash
GOCACHE=/private/tmp/go-agent-optimizer-cache go build -o /tmp/goharness ./cmd/goharness
/tmp/goharness manifest validate targets/gojq/manifest.json
/tmp/goharness manifest validate targets/scc/manifest.json
```

Run `goharness optimize --repo PATH --manifest PATH`, resume with
`goharness optimize --resume CAMPAIGN_DIR` (add `--adk` or `--adk-stub` to
re-attach model agents), and render stored evidence with
`goharness report CAMPAIGN_DIR [--json]`. Add `--adk` to run the ADK graph.
`internal/mcpserver` provides the
typed MCP surface used by `goharness mcp serve --state-root PATH`.

Endpoint configuration (any OpenAI-compatible provider, e.g. OpenRouter):

```sh
OPENAI_API_KEY=...        # provider key
OPENAI_BASE_URL=https://openrouter.ai/api/v1
GOHARNESS_MODEL_COORDINATOR=stealth/ox-alpha
GOHARNESS_MODEL_EXPLORER=openai/gpt-oss-120b
GOHARNESS_MODEL_ANALYST=openai/gpt-oss-120b
GOHARNESS_MODEL_OPTIMIZER=stealth/ox-alpha
GOHARNESS_MODEL_REVIEWER=stealth/ox-alpha
```

Keep credentials in a gitignored `.env`; source it before running.

## Package map

- `internal/agents`, `internal/orchestrator`: Google ADK v2 roles and bounded
  campaign workflow.
- `internal/runner`, `internal/toolchain`, `internal/profile`: subprocess,
  sandbox, A/B scheduling, artifact, pprof, and trace infrastructure.
- `internal/manifest`, `internal/policy`: schema-validated target configuration
  and deterministic acceptance decisions.
- `internal/jobs`, `internal/mcpserver`: cancellable asynchronous work and a
  narrow typed MCP surface with read-only evidence resources.
- `internal/campaign`: persistent in-process campaign engine and reports.
- `targets/gojq`, `targets/scc`: generic-harness validation assets.
