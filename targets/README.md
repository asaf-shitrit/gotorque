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
- `cue/` exercises a config-evaluator workload class comparable to
  go-jsonnet: comprehensions, unification against definitions and
  constraints, and large generated lists, but built from CUE's own
  data-modeling operators (`&`, disjunctions, field constraints) rather than
  a scripting language. Both seeds evaluate the same `fleet.cue` (a
  comprehension over `list.Range` that unifies each entry against a
  `#Service` definition with a tier constraint and a bounded `replicas`
  range): `fleet-export` runs `cue export` over 1,500 services (about
  150-220 ms), and `fleet-eval` runs `cue eval` — which also resolves and
  prints the definitions/constraints, not just the concrete JSON — over 300
  services (about 105-155 ms). The stress seed `fleet-export-profile` scales
  the same comprehension to 42,000 services (about 4.5-4.9 s) for the macOS
  sampler. Local-file evaluation with no external module imports never
  touches `CUE_CACHE_DIR` or `$HOME`, confirmed by running with an
  unwritable `$HOME` and again with none set, so the sandbox's temp-only
  writes need no extra environment allowance. Its own test suite
  (`go test ./...`, 121 packages) took about 103 s and passed cleanly.
- `mdtohtml/` exercises Markdown-to-HTML conversion: parsing, inline
  formatting, tables, fenced code, and (for one seed) table-of-contents and
  heading-ID generation. Both representative seeds read a repeated
  release-notes block on stdin via `stdin_header`/`stdin_repeat` rather than
  an inlined multi-megabyte file: `release-notes-html` repeats the block
  4,000 times (about 3.5 MB in, 100-155 ms) through the default converter,
  and `release-notes-page-toc` repeats it 2,500 times through `-page -toc
  -attributes -headingids` (about 2.2 MB in, 60-85 ms), exercising the TOC
  builder and block-attribute/heading-ID extensions the default path
  skips. The stress seed `release-notes-html-stress` repeats the block
  130,000 times (about 115 MB in, 3.5-5.8 s) for the macOS sampler. The
  module has no test files of its own (`go test ./...` reports "no test
  files" and exits in under 200 ms).
- `fzf/` exercises non-interactive fuzzy and exact filtering: the scorer,
  tiebreaking, and delimiter/field selection, run with `--filter` so no TTY
  is needed. Both representative seeds filter a 50-line generated candidate
  block (file paths built from a small word list) repeated 4,000 times to
  200,000 lines via `stdin_repeat`: `filter-worker` runs the default fuzzy
  `--filter=worker` (about 60-85 ms), and `filter-exact-nth-delim` adds
  `--exact --tiebreak=length,index --delimiter=_ --nth=2` to filter one
  underscore-delimited field with explicit, deterministic tie-breaking
  (about 50-85 ms). Output was confirmed byte-identical across repeated runs
  for both. The stress seed `filter-worker-stress` repeats the same block
  220,000 times (11,000,000 lines, about 3.2-3.9 s) for the macOS sampler.
  Its own test suite (`go test ./...`) took about 6.1 s and passed.

Pointing at `asaf-shitrit/gotorque-targets`, which is not published yet. Both
validate, but neither can run a campaign until that repository exists:

- `dedupe/` exercises line deduplication and map behavior.
- `numstats/` exercises numeric parsing and aggregation.

Every target uses hybrid seed-plus-discovery workloads, representative/
plausible/stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
