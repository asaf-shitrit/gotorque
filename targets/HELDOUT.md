# Held-out targets

These targets exist to judge whether gotorque generalizes. They are never used to develop it.

| Target | Repository | What it exercises |
|---|---|---|
| `s2c` | klauspost/compress | compression, parallel block encoding |
| `csvq` | mithrandie/csvq | SQL over CSV: parsing, grouping, sorting |
| `jd` | josephburnett/jd | structural diff of YAML/JSON |
| `dyff` | homeport/dyff | structural diff of YAML, human-readable report |
| `revive` | mgechev/revive | Go linter: parse, walk, many rules |
| `gofumpt` | mvdan/gofumpt | Go formatter |
| `golines` | segmentio/golines | Go line shortener over a decorated AST |
| `tomlv` | BurntSushi/toml | TOML parser, JSON output |
| `glua` | yuin/gopher-lua | Lua interpreter |
| `otto` | robertkrimen/otto | JavaScript interpreter |

## Rules

- **No campaign runs on a held-out target while the harness is being changed.** Runs happen only
  as an evaluation: a stability sweep, or null candidates, on a committed revision of gotorque.
- **A failure found here is not fixed by tuning against this target.** It is written up, the fix is
  developed and tested on development targets or fixtures, and the held-out sweep is re-run only
  afterwards. If a target has to be retired because it was used for debugging, it moves to the
  development set and a fresh one replaces it here.
- **Stability is claimed on this set, not the development set.** The development targets (every
  other directory under `targets/`) are where harness bugs get found and fixed, so a clean sweep
  there partly measures the fixes themselves.
- The manifests were written from each CLI's source and help text, before any campaign ran on it.
  Workload sizes were calibrated only by timing the unpatched binary.

## Known limitations (as of 2026-09-29)

- **`jd` cannot be measured yet.** Its diff exits 1 whenever the inputs differ, the same convention as
  `diff`, and the runner treats every nonzero exit as a failed run. This is a harness gap, not a jd
  quirk: grep-like tools and linters that report findings through the exit code hit it too. The
  fix is a per-workload expected exit code; until then jd is excluded from held-out sweeps.
- **`otto`'s root package tests do not build under Go 1.26.** `go test` runs vet first, and vet
  rejects two `Example` functions in `documentation_test.go` that name identifiers that do not
  exist. The harness subtracts packages that fail to build on the baseline, so the interpreter's own
  tests never run and otto's behavior gate is weak. Running the suite with `-vet=off` would restore
  them. That is a general question, since newer toolchains' vet can disable any project's tests,
  and it is undecided.

Calibration (unpatched binaries, 5 runs each): every workload is deterministic and exits 0, apart
from jd's diffs, which exit 1. Representative workloads run in 10-160 ms. Test suites take 1-9 s,
except klauspost/compress at about 104 s.
