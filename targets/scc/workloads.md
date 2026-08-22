# scc validation workloads

This asset describes the validation shape for the `boyter/scc` CLI. The
repository is not cloned by this project; the manifest records the build and
workload contract that an adapter can execute against a checked-out revision.

## Execution contract

Build the repository root as `scc`. Each seed materializes its `files` inside a
fresh temporary directory and runs the root CLI with its `args`. Network access
is denied, writes are temporary, and `--no-config` avoids host configuration.
The primary metric is end-to-end wall time, including startup and directory
traversal.

## Workload tiers

- Representative seeds cover a small source tree and a language-filtered scan.
  Their weighted results are eligible for acceptance.
- Plausible seeds add mixed-language and nested-directory classification paths.
  They drive discovery but do not establish headline success.
- Stress seeds expand file count, file size, language mix, and directory depth.
  They can veto unsafe scaling or memory behavior but cannot accept a patch
  alone.

## Discovery expansion

The deterministic generator mutates file contents and tree shape from the
fixed seeds, then sweeps file count, bytes per file, extension mix, and depth.
It records each fixture's provenance. Coverage identifies newly reached
classification and traversal code; CPU, allocation, and execution-trace
profiles distinguish parsing cost from scheduling or filesystem behavior.
Final Linux CI holdouts must be freshly generated and hidden from the patching
agent.

Output, exit code, and declared side effects must match the baseline exactly
after manifest normalization.
