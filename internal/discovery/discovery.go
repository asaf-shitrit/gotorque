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

	"github.com/asaf-shitrit/gotorque/internal/domain"
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
	// representative one discovery samples. Every other seed whose tier the
	// manifest marks acceptance-eligible is sampled next to it.
	Seeds []manifest.SeedWorkload
	// Tiers is the manifest's per-tier workload policy. A seed of an
	// acceptance-eligible tier is sampled, because only those tiers' readings
	// can accept a candidate; a seed's tier weight scales the share its sample
	// carries in the merged hot list. Nil (no declared policy) samples the
	// first seed alone and weighs every sample equally, which is the behavior
	// before tier weighting.
	Tiers map[domain.WorkloadTier]manifest.TierPolicy
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
	// SeedSamples names the workload samples the target-sample hot list was
	// merged from, with the tier weight each carried, so the record says which
	// seeds a function could have ranked from.
	SeedSamples []SeedSample
	// IsolationNotes are what the sandbox policy could not enforce on sampled
	// runs, for the campaign's deduplicated set.
	IsolationNotes []string
	// Events are what discovery has to say, in the order it happened.
	Events []Event
}

// SeedSample is one workload sample the target-sample hot list was merged
// from: which seed ran and the tier weight its contribution carried.
// Without it the record cannot tell a function that ranked because another
// acceptance-eligible seed reached it from one the first seed found alone.
type SeedSample struct {
	Seed   string  `json:"seed"`
	Weight float64 `json:"weight"`
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
	samples, err := r.sampleSeeds(ctx)
	if err != nil {
		return err
	}
	names, weights := sampledHotNamesAndWeights(samples, r.own)
	hot := r.loc.resolve(ctx, "", names)
	r.ev.HotFunctions = hot
	r.ev.Weights = MergeWeights(r.ev.Weights, weights)
	r.ev.UnbufferedWrites = unbufferedWrites(ctx, r.loc, hot, r.own, samples)
	r.ev.SeedSamples = seedSamples(samples)
	r.ev.ProfileSummaryPath = samples[0].Result.RawReport
	return nil
}

// seedSamples names the workloads the hot list was merged from, in the order
// they were sampled, so the persisted state and the report can say which seeds
// a function could have ranked from.
func seedSamples(samples []weightedSample) []SeedSample {
	out := make([]SeedSample, len(samples))
	for i, sample := range samples {
		out[i] = SeedSample{Seed: sample.Seed, Weight: sample.Weight}
	}
	return out
}

// sampleSeeds samples the first seed that can be sampled, every other
// acceptance-eligible seed, and the explorer's variants of the first.
//
// Sampling only the first seed hid whole code paths. actionlint declares two
// representative seeds, and its JSON template reads the source snippet of
// every error, so its quadratic (*Error).getLine was the real cost of
// `-format '{{json .}}'`; discovery sampled `lint-oneline` alone, the hot list
// never named it, and all four attempts went at the oneline path instead. A
// seed whose tier the manifest does not mark acceptance-eligible reaches no
// campaign measurement, so its code is not where the hot list should point.
func (r *run) sampleSeeds(ctx context.Context) ([]weightedSample, error) {
	ladder := NewLadder(r.in, r.sampler)
	defer func() {
		r.ev.IsolationNotes = append(r.ev.IsolationNotes, ladder.Notes()...)
		r.ev.Events = append(r.ev.Events, ladder.TakeEvents()...)
	}()
	seed, result, err := ladder.FirstLiving(ctx)
	if err != nil {
		return nil, err
	}
	// The ladder may have sampled a stress seed because the first seed could
	// not be sampled at all. That sample stands in for the first seed, and
	// carries its weight, so a target that can only be sampled on a larger
	// input still produces the hot list its first seed would have.
	first := weightedSample{Result: result, Seed: seed.ID, Weight: tierWeight(r.in.Tiers, r.in.Seeds[0].Tier)}
	samples := append([]weightedSample{first}, r.sampleAcceptanceSeeds(ctx, ladder, seed)...)
	var variants []manifest.SeedWorkload
	if r.in.Explore != nil {
		variants = r.in.Explore(ctx, seed)
	}
	for _, variant := range ladder.Variants(ctx, variants) {
		// A variant runs the seed's own input in another mode, so it carries
		// the seed's weight rather than one of its own.
		samples = append(samples, weightedSample{Result: variant.Result, Seed: variant.Seed.ID, Weight: first.Weight})
	}
	return samples, nil
}

// sampleAcceptanceSeeds samples every remaining seed whose tier policy marks
// it acceptance-eligible, in manifest order, and records each one that could
// not be sampled. One seed that fails is left out rather than failing
// discovery: the samples that did run still make a hot list.
func (r *run) sampleAcceptanceSeeds(ctx context.Context, ladder *Ladder, sampled manifest.SeedWorkload) []weightedSample {
	var samples []weightedSample
	for i, seed := range r.in.Seeds {
		if i == 0 || seed.ID == sampled.ID || !r.in.Tiers[seed.Tier].AcceptanceEligible {
			continue
		}
		result, err := ladder.seed(ctx, seed, fmt.Sprintf("sample-report-seed-%d.txt", i+1))
		if err != nil {
			ladder.event("workload_sample_skipped", fmt.Sprintf("acceptance-eligible seed %s could not be sampled: %v", seed.ID, err), nil)
			continue
		}
		samples = append(samples, weightedSample{Result: result, Seed: seed.ID, Weight: tierWeight(r.in.Tiers, seed.Tier)})
	}
	return samples
}

// tierWeight is the share a sample of this tier carries in the merged hot
// list. A tier the manifest does not declare weighs nothing, and
// mergeAttributed then counts every sample equally.
func tierWeight(tiers map[domain.WorkloadTier]manifest.TierPolicy, tier domain.WorkloadTier) float64 {
	return tiers[tier].Weight
}

func (r *run) own(symbol string) bool { return ownSymbol(r.in.Packages, symbol) }
