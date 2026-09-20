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

Any OpenRouter slug works for a role: the routing variables take whatever the
endpoint advertises, and a campaign's preflight asks the endpoint's catalogue
before it spends any repository work, so a typo fails with `configured model
"..." for <role> is not advertised by endpoint` rather than a confusing decode
error later. To check a model actually answers in the shape the roles require
before spending a campaign on it:

```sh
GOTORQUE_LIVE_MODEL=deepseek/deepseek-v4.1-flash \
  go test ./internal/agents -run TestLiveModelAnswersARolePrompt -v
```

Free and stealth slugs (`...:free`, `stealth/...`) are worth checking with that
test before trusting them with a campaign, for two reasons. They generally
retain prompts for the provider's own purposes, and a campaign sends source
excerpts and candidate patches, so route a role to one only for targets you are
happy to share. They are also temporary: `stealth/union-alpha` answered role
prompts at zero cost and was retired mid-flight, leaving a campaign that stalled
on ten of thirteen attempts and a later 404 naming its paid successor.

That test calls the model through the same path a campaign uses — streaming,
the retry ladder, fence stripping, JSON decoding and per-role usage accounting —
and skips when the variable or the credential is absent, so CI needs no secret.

`report` also works while a campaign is running. A live campaign holds its
database's exclusive lock, so the report reads the snapshot the engine writes
at startup, after baseline discovery, and after every verdict: expect the
state as of the last of those, and one line per verdict on the progress
stream.

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
