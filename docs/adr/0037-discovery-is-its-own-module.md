# 0037. Discovery is its own module behind a sampler seam

- Status: accepted
- Date: 2026-10-07
- Relates to: [0009](0009-deferred-evaluation-module-seam.md) (the objection to a module that still takes an `Engine`), [0015](0015-jev-explorer.md), [0024](0024-memory-objective-targets.md), [0032](0032-measured-unbuffered-writes.md)

## Context

Discovery was a dozen methods on `Engine` (`internal/campaign/engine.go`, `explore.go`) writing into
`State` as they went. The fallbacks were spread over `engine.go`, `explore.go` and `profile/sample.go`, and only a full
campaign reached most of them:

- `profile.SampleTargetProfile` retried once on `ErrNoFrames`; `sampleSeed` retried at 8x input
  when the target ended too soon; `sampleFirstLiving` moved to the next stress seed;
  `sampleVariant` retried a variant on its input repeated per line; then the benchmark CPU profile,
  which is still needed for PGO when sampling wins; then the alloc profile merged first under a
  memory objective (0024).
- `sampleMacOS` and `sampleLinuxPerf` ran the processes and also parsed the report and decided
  whether it held frames. The Makefile excludes them from the CRAP gate because only their own OS
  can run them, so the parsing and classification inside them were uncovered on the other, and
  every fix to how a sample is read needed a live sampler to reproduce. Their one macOS test of
  the ladder drove `/usr/bin/sample` with `/bin/sh` and skipped everywhere else.
- Ordering was implicit: `writeEvidence` read `State.DiscoveryHotFunctions`, so the hot list had
  to be assigned before the unbuffered-write evidence (0032) was computed, and a campaign killed
  in the middle had persisted half of discovery.

Two failures the next fixes needed were invisible to this shape. `/usr/bin/sample` exits 255 with
"cannot examine process ... for unknown reasons, even though it appears to exist" when the target
ends while it attaches, which is the race `ErrTargetExitedEarly` names, but the message was not
mapped to it: s2c and csvq (held-out), gron and yq fell back to library benchmarks every sweep
(issue 81). And a sample of nothing but parked threads counted as a successful profile with zero
hot functions: the first pup campaign flagged no target and finished before one candidate
(issue 79).

## Decision

The sampler is a port, `profile.Sampler`. An adapter only runs processes and returns a
`profile.Transcript`: the report text, the sampler process's exit status and output, whether the
target had already exited when the sampler was about to attach, and the isolation notes. A pure
`profile.Classify(Transcript)` maps it to a sample or to a typed `*profile.SampleError`: exited
early, no frames, idle, unavailable or failed. The sentinels the retries key off (`ErrNoFrames`,
`ErrTargetExitedEarly`, and the new `ErrIdle`) still match with `errors.Is`. The adapters
(`MacOSSampler`, `LinuxPerfSampler`) shrink to process plumbing, still excluded from the CRAP gate
by name; helpers only they call take their prefix. A scripted adapter, `profile.Replay`, returns
transcripts recorded from real runs, so the classifier and the whole ladder are tested on any OS.

Discovery is a package, `internal/discovery`, not a file set. It takes no `*Engine`:
`Run(ctx, Inputs, profile.Sampler, *toolchain.Toolchain) (Evidence, error)`. `Inputs` is plain data
(binary, command, seeds, an explorer function, the sandbox policy, repository, build package, the
module's packages and `TargetImports`, whether the objective is memory, the campaign directory).
`Evidence` is the hot list, weights, unbuffered writes, summary and PGO paths, profile source,
isolation notes and the events to save. `runDiscoveryStep` applies it to the unchanged persisted
`State` fields, so a saved campaign resumes, and saves the events. The ordering is gone: the hot
list is a parameter of the write evidence, and nothing reaches state before discovery finishes.

The explorer stays in `campaign`, because it asks Jev (0015); it reaches discovery as
`Inputs.Explore`, a function from the sampled seed to option variants. `profile.Sample` retries an
empty or idle call graph once at the same size; `discovery.Ladder` holds the rest (the seed at 8x
input when its inputs are declared repeatable, the next stress seed, a variant on its input per
line).

Two behaviours changed in the same piece of work, each with a test that failed first. `sample`'s
"cannot examine process" with "no longer appears to be running" or "for unknown reasons, even
though it appears to exist" classifies as exited early, while the same message for a refused
permission stays a failure. A sample with no frame of package `main` or of a package under a
hosted path (`github.com/...`, `golang.org/x/...`) classifies as idle and takes the ladder an
empty call graph takes, with the event "sample caught the target idle". A stopped campaign
returns the context's error from `Run`, so the engine no longer marks discovery complete with
"no source" and a resume rediscovers.

## Consequences

The deletion test passes: delete the module and the ladder, the fallback order, the location
resolution and the benchmark profiling reappear in the engine, which is where they sprawled. A
new way for a sample to fail is a recorded transcript and a `Classify` case. ADR 0009's objection
(a module that still takes an `Engine` concentrates nothing) does not apply: `Inputs` is plain
data and the package imports nothing from `campaign`.

Events are now saved after discovery, in order, rather than as each happens; the explorer's
events (`workloads_explored`) are still saved immediately by the campaign, so they come first, as
they did.

The idle rule is read from the transcript alone, so it cannot know the module's own package
paths: a module path with no dot is read as the standard library, and a program of that kind
that never reaches package `main` would sample as idle. Every campaign target has a hosted path
and a `main` command. A thin sample that still holds a `main` frame (a pup run recorded 36
samples, all but one of them waiting) is not idle by this rule and yields one hot function.

No Linux transcript is recorded; the Linux adapter and `ParsePerfScript` are tested on
hand-written `perf script` text only.

## Alternatives considered

A file set in `internal/campaign` (rejected: the package boundary is what stops discovery
reaching for `e.state`, and it has no import cycle to avoid). Classifying inside the adapters
with a hook for tests (rejected: the adapters are the part only one OS can run). Idle decided in
discovery with the module's package list (rejected for now: the ladder would then need a second
channel for "a sample that is not a sample"; the transcript-only rule covers the recorded case;
a minimum sample weight is the open question for the thin-sample class). Sampling every representative seed
(issue 75) and generic method symbols with spaces (issue 80) are separate problems and left out.
