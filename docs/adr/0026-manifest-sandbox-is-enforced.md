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
- **`max_processes`/`max_memory_bytes`.** `runner.WrapWithResourceLimits` wraps the command with
  `/bin/bash -c 'ulimit -u N; ulimit -v K; exec "$0" "$@"' <path> <args>` (bash, not `/bin/sh`:
  `ulimit -v` is a bash extension). This runs before any local-isolation wrapper
  (`sandbox-exec`/`bwrap`), and rlimits survive `exec`, so the limit applies to the whole chain,
  not just the shell. `MaxProcesses` uses `RLIMIT_NPROC` (`ulimit -u`) on both platforms.
  `MaxMemoryBytes` uses `RLIMIT_AS` (`ulimit -v`) on Linux only; macOS is treated as unsupported
  without attempting it (widely-reported kernel behavior, not probed per run). Both probe
  capability once per process (`processLimitSupportedFn`/`memoryLimitSupportedFn`, the same
  sync.Once pattern as the existing bwrap probes) so an environment that cannot set a given rlimit
  is detected once, not on every run. `Manifest.SemanticValidate` now also rejects a negative
  `max_memory_bytes` in Go, not only at the JSON-schema boundary.
- **Degradation is recorded, not silent.** Every gap above — a bwrap network-namespace probe
  failing, a filesystem scope gotorque cannot narrow, an rlimit that could not be set — becomes a
  string on `domain.RunResult.IsolationNotes` (and `profile.SampleResult.IsolationNotes` for the
  sampler). `Engine.recordIsolationNotes` folds these, deduplicated, into
  `State.SandboxIsolationNotes`, and `report.go`'s new `writeSandboxIsolationNotes` prints a
  "Sandbox isolation" section, in the same style as the existing "Degraded roles" section: evidence
  gathered under a listed note is not equivalent to a fully isolated run.

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
the unchanged testing escape hatch. `TestWrapWithResourceLimitsAddsUlimitClauses`,
`...NoopWithoutLimits`, and `...NotesWhenUnsupported` cover the rlimit wrapping and its degraded
path; `TestMemoryLimitNotEnforceableOffLinux` checks the platform gate. `TestReadAndWriteScopeNotesNameTheGap`
and `TestLocalIsolationNotesLinuxNetworkDegrade`/`...DarwinIsAlwaysEmpty` (the latter reusing the
existing fake-bwrap harness from `isolate_test.go`) cover the isolation-note surfacing.
`internal/manifest/manifest_test.go`'s `TestSemanticValidateRejectsNegativeMaxMemoryBytes` covers
the new Go-level validation. The full suite and `make lint`/`make crap-check` pass; a stub campaign
against `targets/gron/manifest.json` was run end to end to confirm discovery, the test gate, and
workload measurement all still succeed under the new policy plumbing.

## Consequences

A manifest's `sandbox.network: "allow"` now actually allows network for that target's workloads;
every existing manifest ships `"deny"`, so no shipped campaign's behavior changes. `max_processes`
and `max_memory_bytes` are now attempted via rlimits where the platform supports them, which is a
best-effort, per-user-account (`RLIMIT_NPROC`) or Linux-only (`RLIMIT_AS`) bound, not a container-
grade one; a manifest author relying on it to stop a runaway target should not treat it as a hard
ceiling on any platform, and the isolation notes say so on every affected run. `filesystem.read`
and the `"manifest_paths"` scope remain aspirational until the schema carries a path list to bind
against — a real gap, now visible in every report instead of silently assumed handled. The
profiler's direct-sampling path (macOS `sample`, Linux `perf`) still cannot apply network or
filesystem isolation at all, because it needs a live PID rather than an exec wrapper; it now at
least shares the same environment and resource-limit policy as a measured run.
