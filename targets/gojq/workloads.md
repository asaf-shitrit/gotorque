# gojq validation workloads

This asset describes the validation shape for the `itchyny/gojq` CLI. The
repository is not cloned by this project; the manifest records the build and
workload contract that an adapter can execute against a checked-out revision.

## Execution contract

Build `./cmd/gojq` as `gojq`. Each seed invokes the root CLI with its `args`
and supplies `stdin` as the input document. Network access is denied and the
CLI may write only to the harness temporary directory. The primary metric is
end-to-end wall time, including process startup and stdin/stdout I/O.

## Workload tiers

- Representative seeds cover projection and numeric array mapping. Their
  weighted results are eligible for acceptance.
- Plausible seeds cover nested reductions inferred from the query language.
  They help discover hot paths but do not establish headline success.
- Stress seeds are expanded into larger arrays, deeper objects, malformed
  documents, and boundary numeric values. They can veto an unsafe change but
  cannot accept one alone.

## Discovery expansion

The deterministic generator may mutate query operators and JSON shapes,
preserving a fixed seed. It should sweep document sizes and nesting depth,
reuse valid inputs as mutation parents, and record provenance for each case.
Coverage identifies newly reached code; CPU and allocation profiles then rank
executed paths. Final Linux CI holdouts must be freshly generated and hidden
from the patching agent.

Output is normalized exactly. A candidate must preserve exit status, stdout,
stderr, and all declared side effects.
