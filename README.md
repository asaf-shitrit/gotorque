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

- The optimizer is the only model role, run over any OpenAI-compatible
  endpoint. Code, not a model, picks what to optimize: discovery profiles the
  target's own workloads, and TypeSafe Jev classifies the hot functions'
  causes, checks each patch for behaviour hazards and picks the option variants
  discovery samples, so the optimizer is handed one function, a cause and a
  remedy.
- The optimizer returns whole function source; deterministic code turns it
  into a diff against the campaign's base revision, so a model never has to
  get hunk positions right.
- Candidate evaluation: diff normalization, a patch-shape check, isolated Git
  worktrees, release-equivalent builds, and the target's own test suite as a
  gate. A patch can never edit the tests, testdata or dependency files that
  judge it.
- Behavior gating: byte-exact or order-insensitive stdout comparison across
  every A/B repetition, plus each seed's expected exit code.
- Statistics, not single runs: interleaved A/B pairs, benchstat's rank test,
  and a confirmation series before any regression or guardrail breach
  rejects. Borderline accepts are measured again before they stand.
- Deterministic acceptance policy. Agents advise; code decides. Every verdict
  is persisted with its reasons and metric comparisons.
- `verify` re-measures accepted patches from their recorded diffs;
  `--null-candidates` measures the harness's own false-accept and
  false-reject rates with patches that change nothing; `scorecard` sums
  verdicts across campaigns.
- bbolt-backed campaign state, mid-run resume, content-addressed artifacts,
  Markdown/JSON reports.
- Sandboxed execution by default: network denial via bubblewrap
  (`--unshare-net`) on Linux and `sandbox-exec` on macOS, writes restricted
  to campaign directories.

## Status

Ten targets are held out from development (`targets/HELDOUT.md`) and used
only to judge whether the harness generalizes. On the two most recent
held-out sweeps, every accepted patch held when `verify` re-measured it over
60 pairs, and 70 null candidates produced no false accept. Most campaigns end
inconclusive: the harness is built to say "not proven" rather than accept
noise.

## Install

Requires Git and GNU patch at run time. `benchstat` is optional.

Prebuilt binaries for Linux and macOS (amd64, arm64) are attached to every
[release](https://github.com/asaf-shitrit/gotorque/releases) from v0.2.0. With Go 1.26+:

```sh
go install github.com/asaf-shitrit/gotorque/cmd/gotorque@latest
```

From source:

```sh
git clone https://github.com/asaf-shitrit/gotorque
cd gotorque
go build -o /tmp/gotorque ./cmd/gotorque
```

## Usage

Validate a target manifest:

```sh
gotorque manifest validate targets/gojq/manifest.json
```

Run a model-driven campaign:

```sh
export OPENROUTER_API_KEY=sk-or-v1-...

# Optional. The optimizer defaults to deepseek/deepseek-v4.1-flash.
export GOTORQUE_MODEL_OPTIMIZER=deepseek/deepseek-v4.1-flash

gotorque optimize \
  --repo /path/to/target-repo \
  --manifest targets/gojq/manifest.json \
  --adk

gotorque report <campaign-dir>
gotorque verify <campaign-dir>      # re-measure its accepted patches
```

`--tradeoff speed|lean` and `--allow metric=percent` say what a campaign may
give up for its improvement; `--history <dir>` skips targets an earlier
campaign on the same revision already measured.

`--adk` needs `OPENROUTER_API_KEY` for both the optimizer model and Jev. Without
an endpoint, `--adk-stub` runs the full pipeline with a stub optimizer and a
no-network Jev stub, which makes it usable in CI. Resume an interrupted campaign
with `optimize --resume <dir> --adk`.

Any OpenRouter slug works for the optimizer: `GOTORQUE_MODEL_OPTIMIZER` takes
whatever the endpoint advertises, and a campaign's preflight asks the endpoint's
catalogue before it spends any repository work, so a typo fails with
`configured optimizer model "..." is not advertised by endpoint` rather than a
confusing decode error later. To check a model actually answers in the shape the
optimizer requires before spending a campaign on it:

```sh
GOTORQUE_LIVE_MODEL=deepseek/deepseek-v4.1-flash \
  go test ./internal/agents -run TestLiveModelAnswersAnOptimizerPrompt -v
```

Free and stealth slugs (`...:free`, `stealth/...`) are worth checking with that
test before trusting them with a campaign, for two reasons. They generally
retain prompts for the provider's own purposes, and a campaign sends source
excerpts and candidate patches, so route the optimizer to one only for targets you are
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

Releases are cut with GoReleaser (pinned in the Makefile): `make
release-snapshot` builds the archives into `dist/` without publishing; tag,
push the tag, then `make release NOTES=<file>` publishes the GitHub release
with hand-written notes.

Architecture details are in [docs/architecture.md](docs/architecture.md).
License: MIT ([LICENSE](LICENSE)).
