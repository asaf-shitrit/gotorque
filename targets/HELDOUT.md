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

## Onboarding findings (fixed)

Setting these targets up found two general gaps, both fixed on development fixtures rather than by
tuning the targets:

- **Nonzero-exit CLIs could not be measured.** jd's diff exits 1 whenever the inputs differ, and the
  runner treated every nonzero exit as a failed run. Seeds now declare `exit_code` (#56); jd's are 1.
- **A newer toolchain's vet stopped otto's tests building.** Go 1.26 vet rejects two `Example`
  functions in `documentation_test.go`, which silently removed the interpreter's 565 tests from the
  gate. Every `go test` the harness runs now passes `-vet=off` (#57).

Calibration (unpatched binaries, 5 runs each): every workload is deterministic and exits with its
declared status. Representative workloads run in 10-160 ms. Test suites take 1-9 s, except
klauspost/compress at about 104 s.
