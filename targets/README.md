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
- `csvtk/` (`shenwei356/csvtk`, package `./csvtk`) exercises CSV parsing,
  group-by statistics, multi-key sort, hash join, regex-driven column
  derivation, and composite-key frequency counting over a deterministically
  generated `id,dept,val` employee table, using `stdin_header`/`stdin_repeat`
  to keep the manifest compact. `employee-summary-by-id` groups a 2,000-row
  block by its unique id column (2,000 groups, about 30-150 ms) and is stored
  without repetition, since repeating it would collide ids and defeat the
  unique-key grouping it measures. `employee-sort-by-value`,
  `employee-department-join`, and `employee-mutate-dept-upper` repeat the same
  2,000-row block 25 times (50,000 rows; about 35-140 ms each), which is safe
  because sort, join, and mutate don't care about row uniqueness.
  `employee-freq-composite` repeats a 3,000-row block of distinct
  `(id,dept,val)` triples 10 times (30,000 rows, about 55-135 ms); freq then
  counts 3,000 distinct composite keys at 10 occurrences each rather than
  30,000 singleton keys, a narrower key space than an equivalent fully-unique
  table, called out in the workload's own description. The stress seed
  `employee-summary-stress` scales the same grouped summary to a 45,000-row
  unique block (about 3.0-4.0 s); csvtk's `summary` verb pays cost per
  distinct group rather than per row and, unlike seqkit and miller, does not
  accept repeated positional file arguments, so this seed cannot be
  compressed with `stdin_repeat` and is the reason `csvtk/manifest.json`
  (about 860 KB) is the one target in this set that stays above the ~500 KB
  most others keep under.
- `seqkit/` (`shenwei356/seqkit`, package `./seqkit`) exercises FASTA
  statistics, reverse-complementing, motif search, tabular conversion, and
  sequence deduplication over deterministically generated DNA reads (fixed
  PRNG seed, no biological randomness reaching the tool), compacted with
  `stdin_repeat`. `fastx-stats` and `fx2tab-convert` repeat a 100-read, 200bp
  block 20 times (2,000 reads, about 17-30 ms); `reverse-complement`,
  `grep-motif`, and `rmdup-sequences` repeat a 150-read, 150bp block (with a
  duplicate injected every 12 records) 80 times (12,000 reads, about
  25-90 ms). Reverse-complementing and motif search are per-record and
  unaffected by the repetition, but `rmdup-sequences` collapses the whole
  input to roughly the 150-read block's own ~137 distinct sequences rather
  than the ~11,000 a genuinely mostly-unique 12,000-read corpus would leave;
  its description says so. The stress seed `rmdup-stress` reconstructs the
  same 12,000-read fixture via a fixture-file `repeat` and passes it 300
  times as separate positional arguments (seqkit concatenates multi-file
  input), about 3.6-4.0 s, without a large inlined fixture or a huge repeat
  count.
- `miller/` (`johnkerl/miller`, package `./cmd/mlr`, binary `mlr`) exercises
  CSV-to-JSON conversion, grouped `stats1` aggregation, numeric `sort`,
  `filter`, and `put` DSL evaluation over the same generated `id,dept,val`
  table as csvtk, compacted with `stdin_repeat`. None of miller's seeds need
  a unique key: `stats1` groups by the low-cardinality `dept` column (8
  groups) and the rest are row-independent, so a 2,000-row block repeated
  30-35 times (60,000-70,000 rows) measures the same work a genuinely
  distinct table would, in about 30-110 ms per seed. The stress seed
  `sort-stress` reconstructs a 60,000-row fixture via a fixture-file `repeat`
  and passes it 45 times as separate positional arguments (mlr concatenates
  multi-file input), about 3.6-3.7 s, without a second large fixture.

`csvtk`, `seqkit`, and `miller` generate their seed data programmatically
from a fixed PRNG seed rather than embedding realistic corpora; the block
generators live only in the tooling used to build each manifest, not in the
repository.

Pointing at `asaf-shitrit/gotorque-targets`, which is not published yet. Both
validate, but neither can run a campaign until that repository exists:

- `dedupe/` exercises line deduplication and map behavior.
- `numstats/` exercises numeric parsing and aggregation.

Every target uses hybrid seed-plus-discovery workloads, representative/
plausible/stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
