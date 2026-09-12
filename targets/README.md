# Validation targets

These are source-repository manifests and workload descriptions, not vendored
repositories. They contain no branches, pull requests, or remote checkout
state. See [`docs/target-manifest.md`](../docs/target-manifest.md) for the
schema, and `gotorque manifest validate PATH` to check one.

Pointing at live upstreams:

- `gojq/` exercises CPU, parsing, interpretation, and allocation behavior in
  a deterministic JSON CLI.
- `scc/` exercises filesystem traversal, classification, allocations, and
  scaling across generated source trees.
- `gron/` exercises JSON flattening and output formatting. Its benchmarks live
  in the module's root package rather than under `./cmd`, which is why
  discovery profiling widens past the target package.

Pointing at `asaf-shitrit/gotorque-targets`, which is not published yet. Both
validate, but neither can run a campaign until that repository exists:

- `dedupe/` exercises line deduplication and map behavior.
- `numstats/` exercises numeric parsing and aggregation.

Every target uses hybrid seed-plus-discovery workloads, representative/
plausible/stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
