# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

gotorque is an optimization harness for Go CLIs. AI agents propose small source
patches; deterministic Go code applies them in isolated Git worktrees, builds,
runs the target's own test suite, measures baseline vs. candidate with
interleaved A/B runs, and accepts only when the improvement is statistically
supported and no guardrail regresses.

The governing invariant: **agents advise, code decides.** No model output may
produce a terminal judgment. Every acceptance, rejection, or inconclusive
verdict comes from `internal/campaign/candidate_eval.go` or `internal/policy`.
When adding an agent-facing feature, keep the decision on the deterministic side
of that line.

## Commands

```sh
make lint                                        # golangci-lint (gocyclo + gocognit only)
GOCACHE=/private/tmp/gotorque-cache go test ./... # full suite
go test ./internal/policy/ -run TestEvaluate -v   # single test
go build -o /tmp/gotorque ./cmd/gotorque
```

Requires Go 1.26+, Git, and GNU patch. `benchstat` and `bubblewrap` (Linux) are
optional; the code degrades rather than failing when they are missing.

CLI surface (`internal/cli/root.go`):

```sh
gotorque manifest validate targets/gojq/manifest.json
gotorque optimize --repo /path/to/repo --manifest targets/gojq/manifest.json --adk-stub
gotorque optimize --resume <campaign-dir> --adk
gotorque report <campaign-dir> [--json]
```

`--adk-stub` runs the whole pipeline with deterministic stub agents and no
network — that is the fast way to exercise engine changes end to end, and it is
what CI runs. `--adk` needs `OPENROUTER_API_KEY` (see `.env`, gitignored) and
spends tokens.

## Lint gates

`.golangci.yml` enables exactly two linters: `gocyclo` (max 10) and `gocognit`
(max 15), both enforced in CI. When a function trips one, split it — do not
raise the thresholds. Recent commits (`afda33e`) follow that pattern: extract
the inner loop body into a named helper.

## Architecture

Read `docs/architecture.md` before non-trivial engine work; it is detailed and
deliberately kept current. Keep it that way — a doc-sync commit (`8e92b07`)
exists because sixteen engine commits landed without touching it.

Flow: CLI → `internal/campaign` engine → ADK workflow graph
(`internal/orchestrator`) alternating agent nodes with deterministic nodes:

```
inspect_repository → coordinator → explorer → run_discovery → analyst
  → merge_analysis → optimizer → evaluate_candidate → reviewer
  → apply_policy → route_campaign (loop or finalize)
```

Package map:

- `internal/campaign` — the engine. `engine.go` owns campaign lifecycle, bounds,
  and discovery profiling; `candidate_eval.go` is the deterministic evaluation
  loop (normalize → worktree → build → test gate → A/B measure → stats);
  `adk.go` bridges engine state into the ADK graph; `store.go` is bbolt state;
  `excerpts.go` feeds real source windows to the optimizer.
- `internal/orchestrator` — graph construction, node wiring, service interfaces.
- `internal/agents` — role definitions, OpenAI-compatible provider, model
  routing, and the model-boundary leniency layer (`fence.go`, `decode.go`,
  `types.go`). This layer only removes parse failures; it never relaxes policy.
- `internal/policy` — pure acceptance decision. No filesystem, process, or
  network access; keep it that way.
- `internal/candidate` — unified-diff normalization, validation, worktrees.
- `internal/toolchain` — allowlisted wrappers for `go`, `git`, `pprof`,
  `benchstat`. It deliberately exposes no general shell API; add a typed method
  rather than a generic `Run(string)`.
- `internal/runner` — sandboxed workload execution (bubblewrap / `sandbox-exec`).
- `internal/profile` — benchmark/CPU profiling, symbol filtering, source
  position annotation.
- `internal/manifest` — target manifest types, defaults, embedded JSON schema.

### Things that are easy to break

- **Diff normalization** (`internal/candidate/normalize.go`) — blank context
  lines in unified diffs are a single space and get trimmed in transit. The
  hunk-termination rules there encode hard-won cases; change them with tests.
- **Model-boundary decoding** (`internal/agents/fence.go`) — a reasoning-only
  turn must become an error, not an empty yield, or ADK kills the run with
  `ErrMultipleOutputs`.
- **Campaign bounds** (`engine.go`) — `max_duration` is charged against
  persisted `ElapsedRunTime` and polled on the wall clock, because Go's
  monotonic timers stop across machine suspend.
- **Resume** — `--resume DIR` requires re-supplying `--adk` or `--adk-stub`
  (agent clients cannot be serialized) and rejects `--repo`, `--manifest`, and
  `--campaign-dir`. Anything that must survive resume has to be persisted in
  bbolt; in-graph `CampaignState` is rebuilt on every entry.
- **Profiled source positions** must be rewritten repository-relative; the
  excerpt collector rejects absolute paths.

## Target manifests

`targets/<name>/manifest.json` is the checked-in contract between a Go CLI repo
and the harness; schema in `internal/manifest/schema/target-manifest-v1.json`,
prose in `docs/target-manifest.md`. Unknown fields are rejected. Defaults live
in the loader (3% minimum improvement, 2% guardrail regression ceiling,
12 candidate patches).

CI globs `targets/*/manifest.json`, so a new target is validated automatically.
`dedupe` and `numstats` point at an unpublished repo — they validate but cannot
run a campaign.

`internal/manifest/docs_test.go` loads the JSON block out of
`docs/target-manifest.md` through the real loader, so editing that example or
the schema without keeping them consistent fails the test suite by design.

## Model routing

Per-role model IDs come from `GOTORQUE_MODEL_{COORDINATOR,EXPLORER,ANALYST,
OPTIMIZER,REVIEWER}`, defaulting to `deepseek/deepseek-v4.1-flash` via
OpenRouter (`internal/agents/routing.go`). `OPENROUTER_BASE_URL` overrides the
endpoint. Credentials are never persisted into campaign state.

## Commit messages

Subject lines are imperative and state the effect ("Bind max_duration to the
wall clock, not to process uptime"). Bodies explain the failure mode that
motivated the change and what evidence covers it, not a summary of the diff.
