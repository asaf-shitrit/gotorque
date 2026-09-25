# 0026. The manifest's `sandbox` block is enforced, not just documented

- Status: accepted
- Date: 2026-09-25

## Context

Every target manifest declares a `sandbox` block (`internal/manifest/types.go`'s `Sandbox`:
`network`, `filesystem.read`/`write`, `environment.allow`/`passthrough`, `max_processes`,
`max_memory_bytes`), the loader validates most of it, and `docs/target-manifest.md` describes it
as "the default target policy" that denies network, restricts writes to a temporary directory, and
bounds child processes. None of that reached a running workload. `internal/campaign/engine.go`
built its `runner.New(runner.Options{...})` from `Artifacts`, `SandboxRoot`, and `LocalIsolation`
only; the manifest's `Sandbox` value was read by the loader and never looked at again.
`runner.RunRequest.NetworkAllowed`/`FilesystemAllowed` existed, but every call site set them to
`!e.state.LocalIsolation` — "is real isolation available at all" — not to what the manifest asked
for, so a manifest declaring `"network": "allow"` still ran with network denied whenever isolation
was available, and a manifest could not distinguish itself from any other manifest at runtime.
`max_memory_bytes` was not validated in Go at all (the JSON schema bounded it, but
`Manifest.SemanticValidate` — the check that runs on a `Manifest` built any other way — did not).
Separately, `internal/profile/sample.go`'s direct-sampling path left `exec.Cmd.Env` unset on both
platforms, which means "inherit the calling process's environment" — gotorque's own environment,
including `OPENROUTER_API_KEY` or `AI_GATEWAY_API_KEY` when `--adk` is in use — reaching the
sampled target binary. The sandboxed runner's own environment was already minimal
(`Sandbox.Env()` plus `GOTOOLCHAIN`), so this leak was real only on the direct-sampling path, but
it existed.

## Decision

`internal/runner` gained a `SandboxPolicy` (`policy.go`): `NetworkAllowed bool`, `WriteScope`/
`ReadScope string`, an `EnvironmentPolicy{Allow, Passthrough []string}`, and a
`ResourceLimits{MaxProcesses int, MaxMemoryBytes int64}`. It is set once on `Runner` at
construction (`runner.Options.Sandbox`), not per `RunRequest`: one manifest governs a whole
campaign, so a baseline and a candidate run under identical policy and the A/B stays fair.
`internal/campaign/engine.go`'s new `sandboxPolicy(manifest.Sandbox) runner.SandboxPolicy` builds
it once in `compose` (covering both a fresh start and `--resume`) and is reused for the profiler's
direct-sampling path (`profile.SampleTarget.Sandbox`), so discovery, candidate A/B, the
confirmation series, the PGO lane (all funnel through `seedMeasurementRequest`), explore variants,
and the sampler share one policy.

Per field:

- **`network`.** `Runner.Run` now derives network denial from `r.sandbox.NetworkAllowed` when
  local isolation is available, instead of always denying. `"allow"` skips the deny-network
  profile/namespace; `"deny"` (the default) keeps it. The `TestingUnsafeDisableIsolation` bypass
  (`LocalIsolation: false`) is unchanged: with no enforcement mechanism at all, both network and
  filesystem stay granted, because there is nothing that could deny them.
- **`filesystem.write`.** `"any"` disables the write restriction (`SandboxPolicy.filesystemUnrestricted`);
  `"temp_only"` and `"manifest_paths"` both restrict writes to the run's sandbox directory, the
  behavior local isolation already gave every run. `"manifest_paths"` cannot be told apart from
  `"temp_only"` because the schema carries no path list for it; `writeScopeNotes` records that gap
  as an isolation note rather than silently treating it as more restrictive than it is.
- **`filesystem.read`.** Local isolation already grants broad read access (`sandbox-exec`'s
  `(allow default)`, bubblewrap's read-only bind of `/`) regardless of policy; there is no
  path list to narrow it to. `readScopeNotes` records `"manifest_paths"` and `"none"` as
  unenforced-as-declared, for the same reason.
- **`environment.allow`/`passthrough`.** `runner.BuildEnv` and its callers
  (`runner.discoveryEnv`, `profile.sampleEnv`) build the workload's environment from the sandbox
  base (`HOME`/`TMPDIR`), the union of `Allow` and `Passthrough` copied from gotorque's own process
  environment when set, and any of `RunRequest.AdditionalEnv` whose key is in `Allow` or is one of
  gotorque's own infrastructure variables (`GOTOOLCHAIN`). Nothing else survives. The base wins on
  a name collision: `targets/gron/manifest.json` lists `HOME` under `environment.allow`, and that
  declares `HOME` non-secret, not a request to replace the sandbox's own home directory (pointed
  at a fresh temp directory) with gotorque's. This closes the direct-sampler leak:
  `sampleMacOS`/`sampleLinuxPerf` now set `Cmd.Env` explicitly instead of leaving it nil.
- **`max_processes` is never enforced, on any platform.** The first version of this change wrapped
  the command in `ulimit -u N` (`RLIMIT_NPROC`), on the reasoning that Unix scopes it per user
  account rather than per process tree. That undersold the problem: on Linux, `RLIMIT_NPROC` also
  counts *threads*, not just processes, and every target manifest in `targets/` sets
  `max_processes: 1`. Verified in a Debian container as an unprivileged user:
  `bash -c 'ulimit -u 1; exec ./gobin'` crashes every Go binary with
  `runtime: failed to create new OS thread ... fatal error: newosproc`, because the Go runtime
  starts more than one OS thread before `main` runs, and those threads count against the same
  limit as the target's own goroutine-driven ones. CI ran as root, which is exempt from
  `RLIMIT_NPROC`, so this was invisible until reproduced directly as a non-root user.
  `WrapWithResourceLimits` no longer builds a `ulimit -u` clause at all; `processLimitNote` records
  why on every run that requests it, and the field stays in the manifest schema (still validated
  `>= 1`) purely so that note can name the requested value.
- **`max_memory_bytes` is enforced on Linux only, for a measured or discovery run, not while
  profiling.** `runner.WrapWithResourceLimits` wraps the command with
  `/bin/bash -c 'ulimit -S -v K || exit 125; exec "$0" "$@"' <path> <args>` (bash, not `/bin/sh`:
  `ulimit -v` is a bash extension) when the platform is Linux, `max_memory_bytes` is set, and the
  rlimit is available; on every other platform, or when nothing is enforceable, the command is
  returned completely unchanged — no shell, no wrapper — never merely a no-op clause inside one.
  That matters beyond memory: with `max_processes` no longer producing a clause either, an
  unenforceable-limits run now has *nothing* to wrap, and callers that attach to a live PID
  immediately after starting the target (`profile.sampleMacOS`) depend on that unwrapped exec
  happening with no shell in between. Reproduced 4/4 in a loop before this fix: `sampleMacOS`
  always ran `/bin/bash -c 'ulimit ...; exec "$0" "$@"' <target>` because every manifest sets
  `max_processes: 1`, and `/usr/bin/sample` attached to the bash PID before it had `exec`'d into
  the target, failing with "sample cannot examine process". `-S` sets the *soft* limit, so the
  shell that applies it can still raise it back toward the hard limit if it needs to; the clause
  fails loudly (`|| exit 125`) instead of the old `2>/dev/null`-suppressed form, which let a failed
  `ulimit -v` silently run the target with no bound and no sign anything went wrong.
  `runner.IsResourceLimitFailure` recognizes that exit 125 came from the wrapper shell itself (not
  a workload coincidentally exiting 125), and `Runner.Run` turns it into a distinct error and
  isolation note rather than reporting a bare exit code. The Linux profiler (`sampleLinuxPerf`)
  does not use this wrapper at all: `perf record` attaches to the target directly, so wrapping it
  would bound `perf`'s own address space alongside the target's for no enforcement benefit;
  `runner.ProfilingResourceLimitNotes` records `max_memory_bytes` as unenforced while profiling
  instead. The probe that detects whether `ulimit -S -v` is settable in this environment
  (`memoryLimitSupportedFn`, cached once per process with `sync.Once`, the same pattern as the
  existing bwrap probes) now runs its own `context.Background()`-derived short timeout rather than
  the first caller's `ctx`, so a cancelled or timed-out caller cannot poison the cached result for
  every later caller. `Manifest.SemanticValidate` still rejects a negative `max_memory_bytes` in
  Go, not only at the JSON-schema boundary. Because the wrapper's fixed bash startup happens
  identically on both sides of every A/B pair — Linux with a memory limit set wraps baseline and
  candidate the same way; everywhere else, neither side is wrapped — this does not bias a
  measurement.
- **Degradation is recorded, not silent.** Every gap above — a bwrap network-namespace probe
  failing, a filesystem scope gotorque cannot narrow, an rlimit that could not be set — becomes a
  string on `domain.RunResult.IsolationNotes` (and `profile.SampleResult.IsolationNotes` for the
  sampler). `Engine.recordIsolationNotes` folds these, deduplicated, into
  `State.SandboxIsolationNotes`, and `report.go`'s new `writeSandboxIsolationNotes` prints a
  "Sandbox isolation" section, in the same style as the existing "Degraded roles" section: evidence
  gathered under a listed note is not equivalent to a fully isolated run.
- **Model provider credentials never reach a target's own build or test.** A review of this change
  before it shipped found that `internal/toolchain.Toolchain.run` — which every `go build`/
  `go test`/benchstat call goes through, including the test gate and measurement builds that
  compile and run a model-written candidate patch — passed `os.Environ()` straight through. Under
  `--adk` that process environment carries `OPENROUTER_API_KEY` and, under `--analyst jev`/
  `--reviewer jev`/`--explorer jev`, `AI_GATEWAY_API_KEY`; nothing stopped candidate code compiled
  and executed as part of the test gate from reading either. `toolchain.withoutCredentials` now
  strips those two names outright, plus any variable whose name ends in `_API_KEY`, `_TOKEN`, or
  `_SECRET`, or contains `PASSWORD` (checked case-insensitively), before every command this package
  runs; everything else — `PATH`, `HOME`, `GOCACHE`, `GOPATH`, `GOFLAGS`, `CGO_*` — passes through
  unchanged, since the Go toolchain needs those to build and test a target at all.

`RunRequest.NetworkAllowed`/`FilesystemAllowed` are removed; every call site that used to set them
identically (`e.state.LocalIsolation`-derived) now relies on the Runner-level policy instead.

## Evidence

`internal/runner/policy_test.go`: `TestBuildEnvDropsUnlistedSecrets` sets
`OPENROUTER_API_KEY`/`AI_GATEWAY_API_KEY` on the test process and asserts neither survives into a
built environment whose policy does not list them; `TestBuildEnvPassthroughIsSeparateFromAllow`,
`TestBuildEnvBaseWinsOverPassthrough` (the gron-shaped `HOME` collision), and
`TestBuildEnvOmitsUnsetPassthrough` cover the allow/passthrough union, precedence, and the "never
set" case.
`TestRunHonorsManifestNetworkPolicy` (darwin) asserts the actual `sandbox-exec` profile string
gotorque builds differs between `network: allow` and `network: deny` with local isolation on —
this is the core bug, reproduced and fixed. `TestRunTestingBypassGrantsBothWithoutAPolicy` locks in
the unchanged testing escape hatch. `TestWrapWithResourceLimitsNeverWrapsForProcessLimit` and
`TestWrapWithResourceLimitsDarwinNeverWraps` are the regression tests for the sample-attach race:
`max_processes` alone, and both limits together on darwin, must return the command completely
unwrapped. `TestWrapWithResourceLimitsAddsMemoryUlimitClauseOnLinux` checks the soft limit
(`ulimit -S -v`) and the loud failure (`|| exit 125`, no `2>/dev/null`); `...NoopWithoutLimits` and
`...NotesWhenMemoryUnsupported` cover the no-limits and unsupported-probe paths;
`TestMemoryLimitNotEnforceableOffLinux` checks the platform gate. `TestIsResourceLimitFailure`
locks in that only the wrapper shell's own exit 125 is recognized, not a workload that happens to
exit with the same code unwrapped. `TestProfilingResourceLimitNotes` covers the Linux sampler's
unwrapped path. `TestReadAndWriteScopeNotesNameTheGap` and
`TestLocalIsolationNotesLinuxNetworkDegrade`/`...DarwinIsAlwaysEmpty` (the latter reusing the
existing fake-bwrap harness from `isolate_test.go`) cover the isolation-note surfacing.
`internal/manifest/manifest_test.go`'s `TestSemanticValidateRejectsNegativeMaxMemoryBytes` covers
the new Go-level validation. `internal/toolchain/toolchain_test.go`'s
`TestBuildStripsCredentialsFromEnv` sets `OPENROUTER_API_KEY`, `AI_GATEWAY_API_KEY`, and
representative `_TOKEN`/`_SECRET`/`PASSWORD`-shaped names on the process and asserts none reach a
build invocation, while an unrelated variable still does. On Linux (Docker, `--user 1000:1000`,
cross-compiled `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`), the exact script `WrapWithResourceLimits`
produces for the gron manifest's limits (`max_processes: 1`, `max_memory_bytes: 1073741824`) was
run as a non-root user and completed normally, and the same script with an impossible
`ulimit -S -v` value exited 125 as designed. The full suite and `make lint`/`make crap-check` pass;
a stub campaign against `targets/gron/manifest.json` was run end to end to confirm discovery
(including macOS sampling, now unwrapped), the test gate, and workload measurement all still
succeed under the corrected policy plumbing.

## Consequences

A manifest's `sandbox.network: "allow"` now actually allows network for that target's workloads;
every existing manifest ships `"deny"`, so no shipped campaign's behavior changes.
`max_processes` is declared in every manifest and validated by the schema, but it is not enforced
anywhere, ever — not as a soft attempt, not on any platform — because there is no unprivileged
mechanism that bounds one run's process tree without risking crashing the run itself (or, on
Linux, the harness's own multi-threaded processes sharing the same account limit).
`max_memory_bytes` is a genuine soft bound, but only on Linux and only for a measured or discovery
run; it is never applied to a profiling sample (macOS or Linux), and on any other platform it is
recorded as unenforced rather than attempted. A manifest author relying on either field to stop a
runaway target should not treat it as a hard ceiling anywhere, and the isolation notes say so on
every affected run. `filesystem.read`
and the `"manifest_paths"` scope remain aspirational until the schema carries a path list to bind
against — a real gap, now visible in every report instead of silently assumed handled. The
profiler's direct-sampling path (macOS `sample`, Linux `perf`) still cannot apply network or
filesystem isolation at all, because it needs a live PID rather than an exec wrapper; it now at
least shares the same environment and resource-limit policy as a measured run.
