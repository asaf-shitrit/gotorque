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
export OPENROUTER_API_KEY=sk-or-v1-...

# Optional. Every role defaults to deepseek/deepseek-v4.1-flash; override a
# role only to tier cost against capability.
export GOTORQUE_MODEL_OPTIMIZER=deepseek/deepseek-v4.1-flash

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
make hooks       # one-time per clone: install the pre-commit gate
make lint
make crap        # CRAP report (complexity × coverage)
make crap-check  # same, exits 1 above CRAP 30
GOCACHE=/private/tmp/gotorque-cache go test ./...
```

`make lint` runs golangci-lint with two complexity gates: `gocyclo`
(cyclomatic, max 10) and `gocognit` (cognitive, max 15), plus `gofmt`. All are
enforced in CI; split a function rather than raising the thresholds. Alongside
them runs a curated correctness set: errcheck, staticcheck, govet, unused,
errorlint, nilerr, noctx, contextcheck, exhaustive, gosec, gocritic, revive
(curated), testifylint and others.

Complexity says nothing about whether a test reaches a function, so `make crap`
scores CRAP = CC² × (1 − coverage)³ + CC using `go-crap` (pinned in the
Makefile) over the `go test -coverprofile` output. The suite runs once and no
coverage tooling is duplicated. At `CRAP_THRESHOLD` 30 a CC 9 function with 0%
coverage scores 90, and a fully covered one scores 9, so the gate catches the
uncovered half. CI runs `make crap-check` as a blocking step, and `make hooks`
installs the same three gates in front of every commit. Code that only executes
on another OS is excluded via `CRAP_EXCLUDE`, because its 0% coverage there is a
portability fact rather than an untested decision.

`make hooks` points git at `.githooks/`, so every commit runs the same three
gates in that order: `make lint`, then `make cover` (the unit tests, which also
writes the coverage profile), then `make crap-scan` reading that profile. The
suite therefore runs once per commit, not twice. A failing gate aborts the
commit, so `git commit --no-verify` is the deliberate escape hatch, and CI is the
backstop. `git config --unset core.hooksPath` removes it.

Isolation note: Linux campaigns isolate workloads through bubblewrap. If
the host cannot support it (some nested CI containers), gotorque detects
this once and runs commands unwrapped rather than failing; use an
environment with working bubblewrap when evidence must be fully isolated.

Architecture details are in [docs/architecture.md](docs/architecture.md).
License: MIT ([LICENSE](LICENSE)).
