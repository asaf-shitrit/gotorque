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
- `jj/` exercises `gjson`/`sjson` key-path get, wildcard-read, set, and delete
  over a large JSON array, generated with the manifest-repeat feature
  (`files[].header`/`content`/`repeat`, docs/target-manifest.md) instead of
  inlining megabytes: a 227-byte record repeated 50k-100k times expands to
  8-17 MB at run time from a 5.6 KB manifest. `edit-order-status` sets one
  field near the end of a 100k-record array (about 25-75 ms), and
  `sum-order-totals` wildcard-reads one field from every element (about
  40-45 ms); `delete-order` (plausible) removes one element from a 50k-record
  array. jj's engine is a byte-level scanner that never builds a parse tree,
  so it is fast even at multi-MB scale: reaching the usual 30 ms floor without
  a multi-MB expanded input was not possible (see docs/target-manifest.md's
  guidance on that trade-off), and the stress seed
  `edit-order-status-large` (2.5M records, ~420 MB expanded) only reaches
  about 1.5-2.7 s, short of the macOS sampler's usual 3-6 s band, because
  going further risks the sandbox's memory ceiling once the input and its
  rewritten output are both held at once. `github.com/tidwall/jj` has no test
  files at all (`go test ./...` finishes in about 0.2 s reporting `[no test
  files]`), so its own test suite is not a campaign cost.
- `hclfmt/` exercises `hclwrite.Format` and its `--check` diagnostics pass
  over generated Terraform-shaped HCL (a repeated `aws_instance` resource
  block), also built with manifest-repeat. `format-generated-tf` formats a
  3,000-block file (about 60-90 ms); `check-generated-tf` runs `--check`
  (which parses twice) over a 1,500-block file (about 65-80 ms);
  `format-two-files` (plausible) formats two 800-block files passed as
  separate positional arguments in one invocation. The stress seed
  `format-generated-tf-large` formats a 115,000-block, ~26 MB file (about
  3.1-3.3 s) for the macOS sampler. `github.com/hashicorp/hcl`'s own test
  suite (`go test ./...` at the module root) passes in about 7.3 s.
- `tomljson/` exercises `toml.Decoder` plus `json.Encoder` over generated
  TOML, converting a repeated `[[servers]]` array-of-tables block, also built
  with manifest-repeat. `convert-generated-toml` reads a 14,000-table file
  from a positional argument (about 36-42 ms); `convert-generated-toml-stdin`
  reads a 7,000-table file from stdin instead (about 25-26 ms), exercising the
  no-args stdin path; `convert-small-toml` (plausible) converts a 200-table
  file. The stress seed `convert-generated-toml-large` converts a 1.6M-table,
  ~197 MB file (observed 4.8-7.6 s across runs on a busy machine, centered in
  the macOS sampler's target band). `github.com/pelletier/go-toml`'s own test
  suite (`go test ./...` at the module root) passes in about 3.7 s.
- `ycat/` exercises `lexer.Tokenize` and `printer.PrintTokens` over generated
  YAML (a repeated list entry under `services:`), also built with
  manifest-repeat. Its CLI is a separate Go module (`cmd/ycat`, with a
  `replace` to the root library), so the manifest sets `build.directory`
  (ADR 0023, like chroma). `print-generated-services` colorizes and prints a
  1,400-item file (about 45-60 ms); `print-small-services` (plausible) prints
  a 100-item file. ycat always emits ANSI color: the manually-built escape
  codes in `format()` are unconditional, and the `fatih/color`-based ones
  (line numbers) auto-detect `isatty` and consistently resolve to no-color
  because gotorque always captures stdout as a pipe, never a TTY — verified
  byte-identical across repeated runs, so no color flag or `NO_COLOR` setting
  was needed. The stress seed `print-generated-services-large` prints a
  150,000-item, ~29.7 MB file (about 4.5-5.1 s) for the macOS sampler.
  `github.com/goccy/go-yaml`'s own test suite (`go test ./...` at the root
  module; `cmd/ycat` is a separate module with no tests of its own) passes in
  about 2.3 s.
- `tengo/` exercises a bytecode-VM script interpreter. Since interpreter
  workloads generate their own work instead of reading input files, discovery
  cannot amplify them by input size, so each seed embeds a small self-writing
  `.tengo` script that does one thing at volume: `fib-arithmetic` runs a
  900,000-iteration modulo-accumulation loop plus 26 calls into a recursive
  Fibonacci helper (about 110-125 ms), `string-build-format` builds a
  60,000-character string by repeated `+=` concatenation and formats 4,500
  lines with `fmt.sprintf` (about 200-240 ms), and `map-churn-sort`
  increments a 60-key map across 45,000 insertions and then insertion-sorts
  both its keys and a 1,200-element numeric list, since tengo's stdlib has no
  `sort()` builtin (about 95-120 ms). The stress seed
  `fib-arithmetic-stress` runs `fib-arithmetic`'s loop at 50,000,000
  iterations (about 4.4-4.7 s) for the macOS sampler. `cmd/tengo` builds from
  the single root module, so no `build.directory` is needed. Its own test
  suite (`go test ./...`) passes in about 5.4 s.
- `starlark/` exercises Google's Starlark interpreter (`go.starlark.net`).
  `fib-arithmetic` runs a 700,000-iteration accumulation loop plus 20 calls
  into a recursive Fibonacci function; Starlark disables recursion by
  default, so the seed's args carry `-recursion` (about 105-110 ms).
  `string-build-format` joins 90,000 single-digit strings and then formats
  and joins 20,000 lines with `%`-interpolation, which in this dialect
  supports no width or precision flags (about 75-90 ms).
  `map-churn-sort` populates a 70-key dict across 260,000 insertions and
  sorts both its keys and a 9,000-element numeric list with `sorted()`
  (about 75-100 ms; Starlark dicts are already insertion-ordered, but the
  workload sorts anyway to keep the hot path comparable to the other two
  targets). The stress seed `fib-arithmetic-stress` runs
  `fib-arithmetic`'s loop at 30,000,000 iterations (about 4.2-4.4 s). Its CLI
  builds from `./cmd/starlark` inside the single root module. Its own test
  suite passes in about 6.4 s.
- `yaegi/` exercises a Go-source interpreter: each seed is itself a small
  `.go` program run with `yaegi run`. `fib-arithmetic` runs a
  1,500,000-iteration accumulation loop plus 30 calls into an iterative
  Fibonacci helper (about 110-115 ms). `string-build-format` builds a
  90,000-byte string with `strings.Builder` and formats 20,000 lines with
  `fmt.Fprintf` (about 70-75 ms). `map-churn-sort` increments a 60-key map
  across 220,000 insertions and sorts both its keys and a 12,000-element
  numeric list with `sort.Strings`/`sort.Ints`, since Go map iteration order
  is not guaranteed (about 75-80 ms). The stress seed
  `fib-arithmetic-stress` runs `fib-arithmetic`'s loop at 70,000,000
  iterations (about 4.6-4.8 s). `cmd/yaegi` builds from the single root
  module. Its own test suite is long and, in this environment, fails: `go
  test ./...` on a plain checkout (not just gotorque's copy) takes about
  356 s and several `TestFile`/`TestInterpConsistencyBuild` cases fail with
  `package location ... not in GOPATH`, because those cases resolve
  `_test`-tree imports against `GOPATH` rather than the module, and this
  clone (like most checkouts) does not sit under `$GOPATH/src`. That is a
  pre-existing property of yaegi's suite, not something introduced here; a
  real campaign against this manifest would need that addressed first, since
  gotorque's test gate rejects a candidate when a baseline-passing test does
  not pass, and here the baseline suite does not pass before any patch is
  applied.

Pointing at `asaf-shitrit/gotorque-targets`, which is not published yet. Both
validate, but neither can run a campaign until that repository exists:

- `dedupe/` exercises line deduplication and map behavior.
- `numstats/` exercises numeric parsing and aggregation.

Every target uses hybrid seed-plus-discovery workloads, representative/
plausible/stress tiers, network-disabled temporary sandboxes, exact output
normalization, and the default idiomatic optimization policy.
