# gotorque

Optimization harness for Go CLIs. AI agents propose small source patches;
the harness applies them in isolated worktrees, builds them, runs the
target's own test suite, measures baseline against candidate with
interleaved A/B runs, and accepts a patch only when the improvement is
statistically supported and no guardrail (CPU time, peak memory, binary
size) regresses.

Models are good at guessing optimizations and bad at proving them. This
harness keeps the guessing and mechanizes the proof.

## Features

- Campaign graph of five agent roles (coordinator, explorer, analyst,
  optimizer, reviewer) over any OpenAI-compatible endpoint. Model routing
  is per role, so cheap models can handle high-volume evidence work while
  stronger ones handle synthesis.
- Candidate evaluation: diff normalization, strict `git apply` with GNU
  patch fuzz fallback, isolated Git worktrees, release-equivalent builds.
- Behavior gating on the target's existing test suite plus byte-exact or
  order-insensitive stdout comparison across A/B repetitions.
- Deterministic acceptance policy. Agents advise; code decides. Every
  verdict is persisted with reasons and metric comparisons.
- bbolt-backed campaign state, mid-run resume, content-addressed
  artifacts, Markdown/JSON reports.
- Sandboxed execution by default: network denial via bubblewrap
  (`--unshare-net`) on Linux and `sandbox-exec` on macOS, writes restricted
  to campaign directories.

## Install

Requires Go 1.26+, Git, and GNU patch. `benchstat` is optional.

```sh
git clone https://github.com/asaf-shitrit/gotorque
cd gotorque
go build -o /tmp/gotorque ./cmd/gotorque
```

## Usage

Validate a target manifest:

```sh
/tmp/gotorque manifest validate targets/gojq/manifest.json
```

Run a model-driven campaign:

```sh
export OPENAI_API_KEY=sk-or-v1-...
export OPENAI_BASE_URL=https://openrouter.ai/api/v1
export GOTORQUE_MODEL_COORDINATOR=stealth/ox-alpha
export GOTORQUE_MODEL_EXPLORER=openai/gpt-oss-120b
export GOTORQUE_MODEL_ANALYST=openai/gpt-oss-120b
export GOTORQUE_MODEL_OPTIMIZER=stealth/ox-alpha
export GOTORQUE_MODEL_REVIEWER=stealth/ox-alpha

/tmp/gotorque optimize \
  --repo /path/to/target-repo \
  --manifest targets/gojq/manifest.json \
  --adk

/tmp/gotorque report <campaign-dir>
```

Without an endpoint, `--adk-stub` runs the full pipeline with deterministic
stub agents, which makes it usable in CI. Resume an interrupted campaign
with `optimize --resume <dir> --adk`.

## Target manifests

A manifest describes the repository, build target, command shape, and seed
workloads. See `targets/gojq` and `targets/scc` for examples, and
`docs/target-manifest.md` for the schema.

## Development

```sh
make lint
GOCACHE=/private/tmp/gotorque-cache go test ./...
```

Isolation note: Linux campaigns isolate workloads through bubblewrap. If
the host cannot support it (some nested CI containers), gotorque detects
this once and runs commands unwrapped rather than failing; use an
environment with working bubblewrap when evidence must be fully isolated.

Architecture details are in [docs/architecture.md](docs/architecture.md).
License: MIT ([LICENSE](LICENSE)).
