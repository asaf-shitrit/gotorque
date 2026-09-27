# Validation targets

These are source-repository manifests and workload descriptions, not vendored
repositories. They contain no branches, pull requests, or remote checkout
state. See [`docs/target-manifest.md`](../docs/target-manifest.md) for the
schema, and `gotorque manifest validate PATH` to check one.

Pointing at live upstreams:

- `gojq/` exercises CPU, parsing, interpretation, and allocation behavior in
  a deterministic JSON CLI.
- `scc/` exercises filesystem traversal, classification, allocations, and
  scaling across generated source trees. Its runtime is mostly its own
  startup (a vendored dependency's `init` alone is 1.9 ms), so no source patch
  can reach the minimum improvement, and campaigns end inconclusive.
- `gron/` exercises JSON flattening and output formatting. Its benchmarks live
  in the module's root package rather than under `./cmd`, which is why
  discovery profiling widens past the target package.
- `yq/` exercises YAML and JSON parsing, query evaluation, and printing, and
  about half its CPU time is in the upstream YAML parser, which patches cannot
  touch. A Jev campaign accepted a fix here: compiling a constant regexp once
  instead of once per printed document made `yaml-select-emails` 6.0% faster.
- `chroma/` exercises a lexer-driven syntax highlighter. Its CLI is a separate
  Go module (`cmd/chroma`, with a `replace` to the root library), so the
  manifest sets `build.directory` (ADR 0023). About 40% of its CPU time is in
  the `regexp2` dependency, which patches cannot touch; the first-party levers
  are its lexer iterators and token allocation. Its first Jev campaign measured
  three candidates, all inconclusive.
- `go-jsonnet/` exercises an interpreter: field lookup, thunks, and
  allocation. Its workloads read `.jsonnet` files, which discovery cannot
  amplify, and a 90 ms run is too short for the macOS sampler, so the
  stress seed `fleet-config-profile` runs the same program at 60x (about 5 s)
  for discovery to sample.
- `goawk/` exercises an AWK interpreter: field splitting, regex matching,
  associative-array accumulation, and printf formatting. Its seeds keep the
  embedded CSV/log fixtures small (a 2,500-row CSV, a 1,700-line log) and
  instead pass the same file as multiple positional arguments, since goawk
  processes each so quickly that a single pass would run in a few
  milliseconds: `field-sum-csv` reads its CSV 60 times (about 58-65 ms),
  `regex-log-count` reads its log 40 times (about 27-36 ms),
  `assoc-array-city-totals` 200 times (about 80-90 ms), and `printf-format`
  40 times (about 52 ms). The stress seed `field-sum-stress` reads the CSV
  9,500 times (about 3.3-3.7 s) for the macOS sampler.
- `shfmt/` exercises a shell parser and printer: default formatting, the
  `-i 2` indent width, `-mn` minify mode, and the combined `-ci`/`-sr`/`-bn`
  style flags, all against one generated 150-function Bash script (functions
  with if/elif/else, for, case, process substitution, and 150 backgrounded
  calls plus a wait). shfmt is fast enough that a single format runs in
  7-15 ms; the stress seed `format-default-stress` formats the same script
  1,400 times in one process (about 3.7-3.8 s) for the macOS sampler.
- `minify/` exercises five minifiers in one binary against generated HTML,
  CSS, JS, JSON, and SVG documents (a 400-row table, 300 CSS rules, 250 JS
  functions, a 300-item JSON document, and 350 SVG shapes), each read from
  stdin and each finishing in single-digit milliseconds. Its CLI builds from
  `./cmd/minify` inside the same module as the library (no `build.directory`
  needed, unlike chroma). The stress seed
  `minify-html-bundle-stress` runs `--bundle` over the HTML page 5,300 times
  (about 3.2-3.5 s) for the macOS sampler.

Pointing at `asaf-shitrit/gotorque-targets`, which is not published yet. Both
validate, but neither can run a campaign until that repository exists:

- `dedupe/` exercises line deduplication and map behavior.
- `numstats/` exercises numeric parsing and aggregation.

Every target uses hybrid seed-plus-discovery workloads, representative/
plausible/stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
