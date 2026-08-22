# Validation targets

These are source-repository manifests and workload descriptions, not vendored
repositories. They contain no branches, pull requests, or remote checkout
state.

- `gojq/` exercises CPU, parsing, interpretation, and allocation behavior in
  a deterministic JSON CLI.
- `scc/` exercises filesystem traversal, classification, allocations, and
  scaling across generated source trees.

Both targets use hybrid seed-plus-discovery workloads, representative/plausible/
stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
