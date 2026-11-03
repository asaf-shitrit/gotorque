# Contributing to gotorque

Thanks for helping. Small fixes, tests and docs are welcome. Issues labelled
`good first issue` are the place to start. If you want one, say so in a comment
so nobody else takes it.

You do not need to open an issue first for a small fix. For a larger change,
open an issue and describe it before you write code. The design is shaped by
[ADRs](docs/adr/README.md), and a change that goes against one needs a new ADR.

## Set up

You need Go 1.26 or later, Git, GNU patch and `make`. `benchstat` and, on
Linux, `bubblewrap` are optional. gotorque works without them and CI installs
both.

```sh
git clone https://github.com/asaf-shitrit/gotorque
cd gotorque
make hooks   # one-time: runs the checks below before every commit
go build -o /tmp/gotorque ./cmd/gotorque
```

Domain terms (campaign, seed, candidate, guardrail and the rest) are defined in
[GLOSSARY.md](GLOSSARY.md). Read [docs/architecture.md](docs/architecture.md)
before you change the engine.

## Run the checks

CI runs these, in this order. Run the same ones before you push.

```sh
make lint                # golangci-lint: gocyclo, gocognit, gofmt and the rest
go test -race ./...      # unit tests (CI uses -race; needs cgo)
make crap-check          # runs the suite with coverage, fails above CRAP 30
```

CI then builds the CLI, validates every `targets/*/manifest.json` and runs one
campaign on gojq with a stub optimizer:

```sh
for m in targets/*/manifest.json; do /tmp/gotorque manifest validate "$m"; done
git clone --depth 1 https://github.com/itchyny/gojq /tmp/gojq
(cd /tmp/gojq && git checkout -b pinned && git commit --allow-empty -m pin)
/tmp/gotorque optimize --repo /tmp/gojq --manifest targets/gojq/manifest.json --adk-stub
```

`--adk-stub` needs no network access to a model and no API key. It is the
check expected for an engine change. Do not run `--adk` for a pull request:
it spends OpenRouter credit, and the maintainers do that run when it is needed.

The pre-commit hook (`make hooks`) runs `make lint`, `make cover` and
`make crap-scan`. It is local configuration, so CI is the real gate.
`git commit --no-verify` skips it for one commit.

## Rules that decide whether a pull request is accepted

- **Agents advise, code decides.** No model output may produce a verdict.
  Every accept, reject or inconclusive result comes from
  `internal/campaign/candidate_eval.go` or `internal/policy`. A feature that
  gives the optimizer more information is fine; a feature that lets it
  influence the decision is not.
- **Do not run campaigns on the held-out targets** in
  [targets/HELDOUT.md](targets/HELDOUT.md) while you change the harness, and do
  not tune a fix against a failure one of them showed. Develop and test on the
  other targets or on fixtures.
- **Do not raise the `gocyclo`, `gocognit` or CRAP thresholds.** If a function
  trips one, split it into named helpers or add tests that reach it. Excluding
  a function from the CRAP scan needs a comment saying why it cannot be tested.

Other points that save review time:

- A change to the verdict, the statistics or a guardrail comes with a test that
  fails without it. An accepted design choice goes in a new ADR (next number in
  `docs/adr/`, plus a row in `docs/adr/README.md`).
- Keep `docs/architecture.md` current when you change engine behavior.
- A test package that runs git needs the `TestMain` that clears the git scoping
  environment. See the "Lint gates" section of `CLAUDE.md` for the reason.

## Commits and pull requests

Commit subjects are imperative and state the effect, for example "Bind
max_duration to the wall clock, not to process uptime". The body explains the
failure that motivated the change, not a summary of the diff. Pull requests
are squash-merged, so the PR title becomes the commit subject. Link the issue
with `Fixes #<n>`.

`CLAUDE.md` holds the same setup written for coding agents, and has more detail
on the parts of the engine that are easy to break.

By contributing you agree your work is released under the [MIT license](LICENSE).
