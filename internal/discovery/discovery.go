// Package discovery gathers the hot-path evidence a campaign's analysis starts
// from: which functions of the target's own code a representative run spends
// its time in. It samples the release binary under the platform sampler,
// falls back to the module's benchmarks when sampling cannot work, and keeps
// the benchmark profile the PGO lane needs.
//
// The package knows nothing of the campaign engine. Its inputs are plain data
// and its output is Evidence, which the engine applies to its persisted state
// and whose events it saves.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/asaf-shitrit/gotorque/internal/runner"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// The sources a hot list can come from, as the completed event and the report
// name them.
const (
	SourceTargetSample = "a target sample"
	SourceBenchmark    = "target benchmarks"
	SourceNone         = "no source"
)

// Inputs is everything discovery needs, as plain data. It holds no engine
// state, so a test builds one directly.
type Inputs struct {
	// BinaryPath is the release baseline binary that is sampled.
	BinaryPath string
	// Command is the target's command prefix, placed before each seed's args.
	Command []string
	// Seeds are the manifest's seed workloads, in order; the first is the
	// representative one discovery samples.
	Seeds []manifest.SeedWorkload
	// Explore returns option variants of the sampled seed to sample next to
	// it (the explorer, ADR 0015, which stays in the campaign because it
	// asks Jev). Nil samples the seed alone.
	Explore func(ctx context.Context, seed manifest.SeedWorkload) []manifest.SeedWorkload
	// Sandbox is the campaign's sandbox policy for sampled runs.
	Sandbox runner.SandboxPolicy
	// Repository is the target checkout; Build says what is built from it.
	Repository string
	Build      manifest.BuildTarget
	// Packages are the module's own package import paths: a sampled frame
	// whose package is one of them (or main) is the target's code.
	Packages []string
	// Imports are the module packages the target imports, in the "./pkg" form
	// the benchmark order uses; nil when unknown.
	Imports []string
	// MemoryObjective is set when the campaign's primary metric is peak
	// memory, which adds a benchmark allocation profile to the hot list
	// (ADR 0024).
	MemoryObjective bool
	// Dir is the campaign directory; raw sampler reports and benchmark
	// profiles are kept under it.
	Dir string
}

// Evidence is what discovery found. The campaign applies it to its persisted
// state and saves Events in order.
type Evidence struct {
	// HotFunctions are the hot list's source locations (or bare symbols for
	// what no declaration could be found for), hottest first.
	HotFunctions []string
	// Weights is each function's hotness, keyed by symbol name.
	Weights map[string]float64
	// UnbufferedWrites are the sample's write sites that are in the hot list
	// (ADR 0032).
	UnbufferedWrites []orchestrator.UnbufferedWrite
	// ProfileSummaryPath is the raw sampler report or the pprof summary the
	// hot list came from.
	ProfileSummaryPath string
	// AllocProfileSummaryPath is the allocation profile's summary, set only
	// under a memory objective.
	AllocProfileSummaryPath string
	// PGOProfilePath is the pprof-format benchmark CPU profile, the only
	// input the PGO lane may use. Empty when the module has no benchmarks.
	PGOProfilePath string
	// Source names which profile(s) chose HotFunctions (the Source constants,
	// plus a suffix for the allocation profile).
	Source string
	// IsolationNotes are what the sandbox policy could not enforce on sampled
	// runs, for the campaign's deduplicated set.
	IsolationNotes []string
	// Events are what discovery has to say, in the order it happened.
	Events []Event
}

// Run gathers the evidence. It is best-effort throughout: a profile that
// cannot be had is recorded as an event and the next source is tried, so a
// missing source never fails a campaign. The one error is the context's: a
// campaign stopped inside discovery has no evidence, and reporting "no source"
// instead would have the engine mark the step done, so a resume would skip it.
func Run(ctx context.Context, in Inputs, sampler profile.Sampler, tc *toolchain.Toolchain) (Evidence, error) {
	r := &run{in: in, sampler: sampler, tc: tc, loc: locator{repository: in.Repository, build: in.Build, tc: tc}}
	r.collect(ctx)
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	return r.ev, nil
}

type run struct {
	in      Inputs
	sampler profile.Sampler
	tc      *toolchain.Toolchain
	loc     locator
	ev      Evidence
}

func (r *run) event(kind, message string, data any) {
	r.ev.Events = append(r.ev.Events, Event{Kind: kind, Message: message, Data: data})
}

// collect chooses where the hot list comes from. The measured workloads come
// first, because they are what the campaign is about. A benchmark CPU profile
// weights every benchmark in the module equally regardless of how much it
// resembles the command, so microbenchmarks dominate the hot list and point
// the optimizer at code that cannot move the measured wall time: on gron three
// identifier microbenchmarks put validFirstRune at 36% cumulative while the
// measured workload's own hot frames (write, statements.Less,
// statement.String) never appeared, and the first two candidates of every
// campaign attacked rune classification before reaching the real cost.
// Sampling the release binary on a manifest seed workload profiles the
// execution the primary metric is taken from.
func (r *run) collect(ctx context.Context) {
	sampleErr := r.sampleTarget(ctx)
	switch {
	case sampleErr == nil:
		// Still collect the benchmark profile when the module has benchmarks:
		// the informational PGO lane is built from it, and dropping that lane
		// because sampling won would be a silent feature regression.
		_, _ = r.benchmarkCPUProfile(ctx)
		r.ev.Source = SourceTargetSample
	case r.fallBackToBenchmarks(ctx, sampleErr):
		r.ev.Source = SourceBenchmark
	default:
		r.ev.Source = SourceNone
		return
	}
	r.ev.Source += r.allocationEvidence(ctx)
}

// fallBackToBenchmarks records why sampling failed and tries the module's
// benchmarks instead, reporting whether they gave a hot list. The failure is
// recorded even when the fallback succeeds: held-out csvq fell back twice with
// no trace of why its sample had failed.
func (r *run) fallBackToBenchmarks(ctx context.Context, sampleErr error) bool {
	r.event("discovery_sample_failed", "direct target sampling failed, falling back to benchmarks: "+sampleErr.Error(), nil)
	benchErr := r.profileHotFunctions(ctx)
	if benchErr != nil {
		r.event("discovery_profile_skipped", fmt.Sprintf("direct target sampling unavailable (%s); benchmark CPU profile unavailable (%s)", sampleErr.Error(), benchErr.Error()), nil)
		return false
	}
	return true
}

// allocationEvidence adds a benchmark alloc_space profile to the hot list when
// the campaign's objective is peak memory (ADR 0024), and returns a suffix
// naming that source for the discovery event, or "" when the objective is not
// memory or the module has no benchmarks.
func (r *run) allocationEvidence(ctx context.Context) string {
	if !r.in.MemoryObjective {
		return ""
	}
	if allocSource := r.profileAllocations(ctx); allocSource != "" {
		return " + " + allocSource
	}
	return ""
}

// sampleTarget runs the first representative seed workload against the
// release baseline binary under the platform sampler and records the hottest
// frames as the hot list. The raw sampler report is preserved under
// profile-sample/ in the campaign dir. Strictly best-effort: any failure is
// returned for a skipped event, and the evidence is set only on success.
func (r *run) sampleTarget(ctx context.Context) error {
	if r.in.BinaryPath == "" {
		return errors.New("no baseline binary")
	}
	if info, statErr := os.Stat(r.in.BinaryPath); statErr != nil || info.IsDir() {
		return errors.New("baseline binary missing on disk")
	}
	if len(r.in.Seeds) == 0 {
		return errors.New("manifest defines no seed workloads to sample")
	}
	results, err := r.sampleSeeds(ctx)
	if err != nil {
		return err
	}
	names, weights := sampledHotNamesAndWeights(results, r.own)
	hot := r.loc.resolve(ctx, "", names)
	r.ev.HotFunctions = hot
	r.ev.Weights = MergeWeights(r.ev.Weights, weights)
	r.ev.UnbufferedWrites = unbufferedWrites(ctx, r.loc, hot, r.own, results)
	r.ev.ProfileSummaryPath = results[0].RawReport
	return nil
}

// sampleSeeds samples the first seed that can be sampled, then the explorer's
// variants of it. The first result is the seed's.
func (r *run) sampleSeeds(ctx context.Context) ([]profile.SampleResult, error) {
	ladder := NewLadder(r.in, r.sampler)
	defer func() {
		r.ev.IsolationNotes = append(r.ev.IsolationNotes, ladder.Notes()...)
		r.ev.Events = append(r.ev.Events, ladder.TakeEvents()...)
	}()
	seed, result, err := ladder.FirstLiving(ctx)
	if err != nil {
		return nil, err
	}
	var variants []manifest.SeedWorkload
	if r.in.Explore != nil {
		variants = r.in.Explore(ctx, seed)
	}
	return append([]profile.SampleResult{result}, ladder.Variants(ctx, variants)...), nil
}

func (r *run) own(symbol string) bool { return ownSymbol(r.in.Packages, symbol) }
