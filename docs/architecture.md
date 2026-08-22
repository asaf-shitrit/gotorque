# Architecture

## Responsibility split

Google ADK Go 2.x is the internal orchestration runtime. MCP is the external
control and observation surface. Neither layer reimplements deterministic Go,
Git, profiling, coverage, or statistical tools.

```text
CLI or MCP client
    |
    v
in-process campaign engine ---- asynchronous job manager
    |                              |
    v                              v
ADK workflow graph -------- campaign state and artifacts
    |
    +-- deterministic nodes: inspect, build, run, profile, compare, policy
    |
    +-- specialist agents: explore, analyze, optimize, review
```

## ADK workflow

The workflow is a bounded graph rather than an unconstrained chat loop:

```text
inspect target
  -> propose workload strategy
  -> deterministic workload expansion
  -> discovery run
  -> diagnostic run
  -> hotspot policy gate
  -> hypothesis and candidate patch
  -> deterministic validation
  -> reviewer
  -> hidden holdout measurement
  -> deterministic acceptance policy
  -> accept temporary baseline | reject | mark inconclusive
```

The coordinator chooses among permitted experiments. The final acceptance
transition is always produced by deterministic policy.

## Model routing

Each ADK role receives its model through an injected provider. The default
routing uses Luna for Explorer, Terra for Analyst and Reviewer, and Sol for
Coordinator and Optimizer. This routing is cost/latency configuration only:
all builds, measurements, behavior checks, and acceptance transitions remain
deterministic and model-independent.

## Run modes

- `discovery`: coverage-instrumented execution used to find reachable paths.
- `diagnosis`: pprof or execution-trace collection used to explain cost.
- `measurement`: release-equivalent execution used for authoritative timing.
- `validation`: tests, race detection, fuzzing, differential output, and
  invariants.

Instrumentation results must not be mixed with authoritative performance
measurements.

## Dependency policy

Use maintained libraries aggressively for orchestration, protocols, schemas,
profile parsing, CLI structure, and tests. Invoke `go`, `go tool`, `git`, and
`benchstat` directly when they are the authoritative implementation. All
dependencies are pinned and wrapped behind narrow package interfaces.

## Security boundary

Target commands run without network access and with writes confined to a fresh
temporary directory unless a target manifest explicitly grants additional
capabilities. The MCP server exposes typed operations, not a generic shell.
Candidate changes are isolated in Git worktrees and never published by the MVP.
