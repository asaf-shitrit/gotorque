package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/asaf-shitrit/gotorque/internal/runner"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// profileHotFunctions runs benchmarks under a CPU profile, then summarizes
// the top functions via go tool pprof.
//
// The target package is tried first, then the whole module. A CLI's command
// package usually holds no benchmarks while the library packages it drives do
// (gojq benchmarks its evaluator, not ./cmd/gojq), and a module-wide profile
// still carries exact file and line data for every sampled frame. Without the
// widened attempt those targets silently degrade to the OS sampler, whose
// frames carry no source position at all.
func (r *run) profileHotFunctions(ctx context.Context) error {
	cpuProfile, err := r.benchmarkCPUProfile(ctx)
	if err != nil {
		return err
	}
	return r.summarizeBenchmarkProfile(ctx, cpuProfile)
}

// benchmarkCPUProfile runs the module's benchmarks under -cpuprofile and
// records the profile as the PGO lane's input, returning its path. It is also
// called when the sampler already supplied the hot functions, because the
// informational PGO lane is built from this profile.
func (r *run) benchmarkCPUProfile(ctx context.Context) (string, error) {
	dir := filepath.Join(r.in.Dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	cpuProfile := filepath.Join(dir, "bench-cpu.pb.gz")
	if !r.benchmark(ctx, toolchain.TestRequest{Cpuprofile: cpuProfile, Output: filepath.Join(dir, "bench.test")}) {
		return "", errors.New("no package in the module produced a benchmark CPU profile")
	}
	r.ev.PGOProfilePath = cpuProfile
	return cpuProfile, nil
}

// benchmark runs every candidate package's benchmarks under the profiling
// flags of req, target package first, stopping at the first one that actually
// benchmarked something. The test binary go test writes (Output) must stay out
// of the canonical checkout (see testArgs's -o handling in
// internal/toolchain/toolchain.go).
func (r *run) benchmark(ctx context.Context, req toolchain.TestRequest) bool {
	req.Repository, req.Bench, req.Env = r.in.Repository, ".", []string{"GOTOOLCHAIN=local"}
	for _, pkg := range benchmarkPackageOrder(r.in.Repository, r.in.Build.Package, r.in.Build.Directory, r.in.Imports) {
		req.Packages = []string{pkg}
		result, err := r.tc.Test(ctx, req)
		if err != nil {
			continue
		}
		if strings.Contains(string(result.Stdout), "Benchmark") {
			return true
		}
	}
	return false
}

// summarizeBenchmarkProfile turns a benchmark CPU profile into hot functions
// annotated with repository-relative source positions.
func (r *run) summarizeBenchmarkProfile(ctx context.Context, cpuProfile string) error {
	collector, err := r.collector()
	if err != nil {
		return err
	}
	summary, err := collector.SummarizePprof(ctx, cpuProfile, hotFunctionScanDepth)
	if err != nil {
		return fmt.Errorf("summarize benchmark CPU profile: %w", err)
	}
	r.ev.HotFunctions = r.loc.resolve(ctx, cpuProfile, hotFunctionNames(summary.Functions, hotFunctionBudget))
	r.ev.Weights = MergeWeights(r.ev.Weights, HotFunctionWeights(summary.Functions))
	r.ev.ProfileSummaryPath = summary.RawReport
	return nil
}

func (r *run) collector() (profile.Collector, error) {
	artifacts, err := runner.NewArtifactStore(filepath.Join(r.in.Dir, "artifacts"))
	if err != nil {
		return profile.Collector{}, err
	}
	return profile.Collector{Toolchain: r.tc, Artifacts: artifacts}, nil
}

// profileAllocations runs the module's benchmarks a second time under
// -memprofile and folds the alloc_space profile's hottest allocators to the
// front of discovery's hot list, ahead of any CPU-only function already
// there. It returns the source name for the discovery_profile_completed
// event, or "" when the module has no benchmarks — silent beyond the
// discovery_alloc_profile_skipped event, since discovery still has its CPU
// evidence to work from (ADR 0024).
//
// Only called under the peak_memory_bytes objective.
func (r *run) profileAllocations(ctx context.Context) string {
	dir := filepath.Join(r.in.Dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	memProfile := filepath.Join(dir, "bench-mem.pb.gz")
	if !r.benchmark(ctx, toolchain.TestRequest{Memprofile: memProfile, Output: filepath.Join(dir, "bench-mem.test")}) {
		r.event("discovery_alloc_profile_skipped", "no package in the module produced a benchmark allocation profile; hot list built from CPU evidence only", nil)
		return ""
	}
	locations, weights, summaryPath, err := r.summarizeAllocProfile(ctx, memProfile)
	if err != nil {
		r.event("discovery_alloc_profile_skipped", "summarize benchmark allocation profile: "+err.Error(), nil)
		return ""
	}
	r.ev.HotFunctions = mergeAllocFirst(r.ev.HotFunctions, locations, hotFunctionBudget)
	r.ev.Weights = MergeWeights(r.ev.Weights, weights)
	r.ev.AllocProfileSummaryPath = summaryPath
	r.event("discovery_alloc_profile_completed", fmt.Sprintf("measured %d allocation-heavy functions from the benchmark alloc_space profile", len(locations)), locations)
	return "a benchmark alloc_space profile"
}

// summarizeAllocProfile turns a benchmark heap profile into hot functions,
// ranked by the alloc_space sample index, with repository-relative source
// positions.
func (r *run) summarizeAllocProfile(ctx context.Context, memProfile string) ([]string, map[string]float64, string, error) {
	collector, err := r.collector()
	if err != nil {
		return nil, nil, "", err
	}
	summary, err := collector.SummarizePprofAllocSpace(ctx, memProfile, hotFunctionScanDepth)
	if err != nil {
		return nil, nil, "", err
	}
	locations := r.loc.resolve(ctx, memProfile, hotFunctionNames(summary.Functions, hotFunctionBudget))
	return locations, HotFunctionWeights(summary.Functions), summary.RawReport, nil
}
