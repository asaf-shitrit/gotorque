# 0009. Deferred: give candidate evaluation its own module seam

- Status: deferred
- Date: 2026-09-20

## Context

`internal/campaign/engine.go` is the hottest file in the repository (1300+ lines, in 8 of the last
20 commits) and owns campaign lifecycle, git and build orchestration, discovery runs, profiling,
excerpt assembly, candidate evaluation, the PGO lane and reporting. The file split
(`candidate_eval.go`, `report.go`, `excerpts.go`) is a file seam, not a module seam: evaluation
cannot run without a fully populated `Engine`, so there is no seam to test or change evaluation
alone.

## Decision

Defer the split until a concrete need appears: a second evaluator, or tests that require a fake
store and progress channel rather than a real campaign directory.

## Consequences

The deletion test is conditional here. Extracting a module that still takes an `Engine` concentrates
nothing and adds indirection, which is the failure mode the architecture survey warns about.

## Alternatives considered

Split now (rejected: no evidence it concentrates complexity). Recorded here so a later survey does
not re-suggest it without new evidence.
