# 0023. A target's build may run from a nested module directory

- Status: accepted
- Date: 2026-09-25

## Context

gotorque always runs `go build <package>` from the repository root
(`internal/toolchain.Toolchain.Build`, called from `internal/campaign/engine.go`'s baseline and
coverage builds, `internal/campaign/candidate_eval.go`'s candidate and PGO-lane builds). That
assumes the target package is reachable from the root module's go.mod. Some Go CLIs keep their
`cmd/<name>` as its own module instead, with a `replace ../../` back to the root library so it can
still be built and developed against local root-library changes without the root module depending
on every CLI's third-party flags/config packages. `alecthomas/chroma` is one: `cmd/chroma` has its
own `go.mod` and `go.sum`. Pointing gotorque at it fails immediately: `go build` from the repository
root reports `main module (github.com/alecthomas/chroma/v3) does not contain package
.../cmd/chroma`, because the root module's go.mod does not list that package at all.

## Decision

`target.build` gains an optional, repository-relative `directory` (`internal/manifest/types.go`,
schema `internal/manifest/schema/target-manifest-v1.json`). It must not be absolute and must not
escape the repository (`validateRelativePath`, the same helper fixture paths and normalization file
paths already use). Empty is the default and preserves every existing manifest's behavior exactly:
no `directory` runs from the repository root, as before.

`toolchain.BuildRequest` gains a matching `Directory` field. `Toolchain.Build` resolves the working
directory as `filepath.Join(Repository, Directory)` (re-validated in the toolchain itself, since it
is called directly by tests and must not trust a caller's input) and runs `go build <Target>` from
there; `Target` is then relative to that directory, e.g. `directory: "cmd/chroma"`, `package: "."`.
Every build call site was updated to pass the manifest's `Directory` through: the baseline build and
its coverage twin (`engine.go`'s `build`), the candidate build and the PGO lane's baseline/candidate
builds (`candidate_eval.go`).

The test gate keeps running the root module's `go test ./...` unchanged — it judges the root module,
and a nested module's own tests, if it has any, are not part of that gate. A patch to the root
library still reaches the CLI module through its `replace` directive, so the gate still exercises
code the CLI depends on; it just cannot see a test that lives only inside the nested module.

Protected-path rejection (`internal/candidate/patch.go`) already matches dependency files by
basename (`dependencyFiles[base]`), not by full path, so a nested module's own `go.mod`/`go.sum` were
already off-limits to a candidate patch without any change; `TestRejectProtectedPath` in that package
gained a case naming a nested path to make that explicit.

`dependencyDigests` (`internal/campaign/engine.go`), which verifies nothing about the dependency
graph moved during a campaign, now also digests the nested module's `go.mod`/`go.sum` when
`directory` is set, alongside the root module's files it already covered.

Two more assumptions were checked and left alone, with the gap documented rather than closed:
`inspect` (`go list ./...` for the campaign's package inventory) and discovery's benchmark
profiling (`benchmarkPackageOrder`) both operate on the root module. Inventory is informational.
Benchmark profiling tries the target package first, then falls back to the rest of the root module;
when `directory` is set, the target package is not reachable from the root and is left out of that
first try rather than run against the wrong package or fail loudly, so profiling falls straight to
the root module's own benchmarks (documented in `docs/target-manifest.md`).

## Evidence

- `internal/manifest/manifest_test.go`: a manifest with a relative `target.build.directory` loads;
  an absolute or repository-escaping one is rejected.
- `internal/toolchain/toolchain_test.go`: `Build` with `Directory` set runs `go build` with that
  directory as the working directory, and an absolute or escaping `Directory` is rejected before any
  command runs.
- `internal/campaign`: an engine-level test builds a tiny fixture repository whose CLI is a nested
  module with a `replace ../../`, confirming the whole path — manifest, engine, toolchain — builds it
  correctly.
- `internal/candidate/patch_test.go`: a patch naming a nested `<directory>/go.mod` is rejected the
  same way a root `go.mod` is.
- Manual validation against a real `alecthomas/chroma` clone (`~/projects/gotorque-work/chroma`,
  `cmd/chroma` as its own module) with a draft manifest: `gotorque optimize --adk-stub` builds the
  baseline binary from `cmd/chroma` and runs its workloads. See the accompanying report for the exact
  command and output.

## Consequences

A target manifest can now name a Go CLI that keeps its command package in its own module, without
gotorque needing to vendor it into the root module or restructure the target repository. The
trade-off is scope: the harness's test gate and benchmark discovery still only see the root module,
so a nested module with its own tests or benchmarks gets no coverage from either; that is
acceptable because those modules exist specifically to decouple a thin CLI shell from the root
library gotorque is optimizing, and the root library's own tests still gate every candidate.
