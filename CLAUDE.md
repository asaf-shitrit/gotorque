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
make hooks                                       # one-time: install the pre-commit gate
make lint                                        # golangci-lint (gocyclo + gocognit + gofmt)
make crap                                        # CRAP report: complexity × coverage risk
make crap-check                                  # CRAP gate, exits 1 above CRAP_THRESHOLD
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
network. That is the fast way to exercise engine changes end to end, and it is
what CI runs. `--adk` needs `OPENROUTER_API_KEY` (see `.env`, gitignored) and
spends tokens.

## Lint gates

`.golangci.yml` enables a curated best-practice set on top of `standard`
(errcheck, govet, ineffassign, staticcheck, unused): errorlint, nilerr, nilnil,
noctx, contextcheck, exhaustive, gosec, gocritic, revive, perfsprint,
testifylint, inamedparam and more, plus `gocyclo` (max 10) and `gocognit`
(max 15) and the `gofmt` formatter. All are enforced in CI. When a function
trips a complexity gate, split it. Do not raise the thresholds. Recent commits
(`afda33e`) follow that pattern: extract the inner loop body into a named
helper.

Two deliberate narrowings, both commented in the config: `gosec`'s G204/G304/
G703 are off because they fire on the harness's core job (running a target's
manifest-defined commands through the allowlisted `internal/toolchain` wrapper,
and reading repository paths it was pointed at), and `revive`'s `exported` and
`package-comments` rules are off because architecture lives in
`docs/architecture.md` and this file, not in per-symbol doc blocks.

Complexity alone does not see whether a test reaches the function, so `make
crap` adds that axis: CRAP = CC² × (1 − coverage)³ + CC, computed by `go-crap`
(pinned in the Makefile) from the `go test -coverprofile` output. At
`CRAP_THRESHOLD` 30 a CC 9 function with 0% coverage scores 90, while a fully
covered one scores 9. The gate exists to catch the uncovered half. Coverage is
measured cross-package (`-coverpkg=./...` in `COVERPKG`): a function exercised
by another package's tests is tested, and a per-package profile reports it as
0%. CI runs `make crap-check` blocking; the backlog is at zero, so a new
function above the threshold fails the build. Platform-exclusive code is
excluded (`CRAP_EXCLUDE` in the Makefile): the sampler dispatch picks
`sampleMacOS` on darwin and `sampleLinuxPerf` on Linux, and neither can be
covered on the other OS, so counting them made the gate pass locally and fail
on the identical commit in Linux CI.

Raising coverage on a flagged function is the preferred fix; splitting it is
the fallback, and excluding it (`--exclude` in the Makefile target) needs a
comment saying why it cannot be tested.

Both gates run before every commit: `make hooks` (once per clone) sets
`core.hooksPath` to `.githooks/`, whose pre-commit hook runs `make lint`, then
`make cover` (tests + coverage profile), then `make crap-scan`. Commit aborts on
failure; `--no-verify` is the escape hatch. The hook is local config, so CI
stays the enforcement point for anyone who has not run `make hooks`, so do not
treat a green local commit as proof CI will pass.

Inside the hook, git exports `GIT_INDEX_FILE`, and `GIT_DIR` too when the
commit is made in a linked worktree. A test that runs git directly inherits
them and writes into the repository being committed. From a worktree, this
once made the repository bare, set a test identity in its config, and
committed fixtures onto the branch. The hook unsets them, and every test
package that builds fixture repositories clears `toolchain.GitScopingEnv()`
in its `TestMain`. A new package that shells out to git needs the same
`TestMain`.

## Architecture

Read `docs/architecture.md` before non-trivial engine work; it is detailed and
deliberately kept current. Keep it that way: a doc-sync commit (`8e92b07`)
exists because sixteen engine commits landed without touching it.

Flow: CLI -> `internal/campaign` engine -> ADK workflow graph
(`internal/orchestrator`) alternating agent nodes with deterministic nodes:

```
inspect_repository -> coordinator -> explorer -> run_discovery -> analyst
  -> merge_analysis -> optimizer -> evaluate_candidate -> reviewer
  -> apply_policy -> route_campaign (loop or finalize)
```

Package map:

- `internal/campaign`: the engine. `engine.go` owns campaign lifecycle, bounds,
  and discovery profiling; `candidate_eval.go` is the deterministic evaluation
  loop (normalize -> worktree -> build -> test gate -> A/B measure -> stats);
  `adk.go` bridges engine state into the ADK graph; `store.go` is bbolt state;
  `excerpts.go` feeds real source windows to the optimizer.
- `internal/orchestrator`: graph construction, node wiring, service interfaces.
- `internal/agents`: role definitions, OpenAI-compatible provider, model
  routing, and the model-boundary leniency layer (`fence.go`, `decode.go`,
  `types.go`). This layer only removes parse failures; it never relaxes policy.
- `internal/policy`: pure acceptance decision. No filesystem, process, or
  network access; keep it that way.
- `internal/candidate`: unified-diff normalization, validation, worktrees.
- `internal/toolchain`: allowlisted wrappers for `go`, `git`, `pprof`,
  `benchstat`. It deliberately exposes no general shell API; add a typed method
  rather than a generic `Run(string)`.
- `internal/runner`: sandboxed workload execution (bubblewrap / `sandbox-exec`).
- `internal/profile`: benchmark/CPU profiling, symbol filtering, source
  position annotation.
- `internal/manifest`: target manifest types, defaults, embedded JSON schema.

### Things that are easy to break

- Diff normalization (`internal/candidate/normalize.go`): blank context lines
  in unified diffs are a single space and get trimmed in transit. The
  hunk-termination rules there encode hard-won cases; change them with tests.
- Model-boundary decoding (`internal/agents/fence.go`): a reasoning-only turn
  must become an error, not an empty yield, or ADK kills the run with
  `ErrMultipleOutputs`.
- Campaign bounds (`engine.go`): `max_duration` is charged against persisted
  `ElapsedRunTime` and polled on the wall clock, because Go's monotonic timers
  stop across machine suspend.
- Resume: `--resume DIR` requires re-supplying `--adk` or `--adk-stub` (agent
  clients cannot be serialized) and rejects `--repo`, `--manifest`, and
  `--campaign-dir`. Anything that must survive resume has to be persisted in
  bbolt; in-graph `CampaignState` is rebuilt on every entry.
- Test gate (`internal/candidate/patch.go`, `worktree.go`,
  `internal/campaign/testgate.go`): a patch must never be able to edit what
  judges it. Protected paths (tests, `testdata/`, dependency files) are
  checked in every diff header and again in Git's list of changed files after
  apply, because GNU patch can edit a file validation never saw. A
  baseline-passing test that is skipped or missing rejects the candidate.
- Model streams (`internal/agents/sse.go`): openai-go fails on SSE comment
  keepalives and accepts a stream cut before `response.completed`. Both are
  handled by the body filter, so model calls must keep going through
  `modelClient`.
- The per-node `DeterministicTimeout` covers all of `evaluate_candidate`; it
  is not `minimum_command_timeout` (a per-command floor).
- Profiled source positions must be rewritten repository-relative; the excerpt
  collector rejects absolute paths.

## Target manifests

`targets/<name>/manifest.json` is the checked-in contract between a Go CLI repo
and the harness; schema in `internal/manifest/schema/target-manifest-v1.json`,
prose in `docs/target-manifest.md`. Unknown fields are rejected. Defaults live
in the loader (3% minimum improvement, 2% guardrail regression ceiling,
12 candidate patches).

CI globs `targets/*/manifest.json`, so a new target is validated automatically.
`dedupe` and `numstats` point at an unpublished repo, so they validate but
cannot run a campaign.

`internal/manifest/docs_test.go` loads the JSON block out of
`docs/target-manifest.md` through the real loader, so editing that example or
the schema without keeping them consistent fails the test suite by design.

## Model routing

Per-role model IDs come from `GOTORQUE_MODEL_{COORDINATOR,EXPLORER,ANALYST,
OPTIMIZER,REVIEWER}`, defaulting to `deepseek/deepseek-v4.1-flash` via
OpenRouter (`internal/agents/routing.go`). `OPENROUTER_BASE_URL` overrides the
endpoint.
Optional `GOTORQUE_REASONING_{COORDINATOR,EXPLORER,ANALYST,OPTIMIZER,REVIEWER}`
(`low|medium|high`) sets per-role `reasoning.effort`. Unset sends nothing,
and an invalid value fails the preflight.
Credentials are never persisted into campaign state.

## Commit messages

Subject lines are imperative and state the effect ("Bind max_duration to the
wall clock, not to process uptime"). Bodies explain the failure mode that
motivated the change and what evidence covers it, not a summary of the diff.
