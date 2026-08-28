package campaign

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"example.com/gotorque/internal/candidate"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/runner"
	"example.com/gotorque/internal/toolchain"
)

// measurementRepetitions is the interleaved A/B sample count per workload.
// Seven pairs give the t-test enough degrees of freedom while keeping one
// candidate cycle inside the manifest's command-timeout budget.
const measurementRepetitions = 7

// evaluateCandidate runs the deterministic half of the candidate loop:
// validate the proposed diff, apply it in an isolated worktree, build,
// gate on the upstream test suite, then measure baseline and candidate
// binaries interleaved on representative seed workloads. Every terminal
// judgment stays here or in policy; the model never self-approves.
// evaluateCandidate is invoked through the orchestrator CandidateService adapter.
func (e *Engine) evaluateCandidate(ctx context.Context, req orchestrator.CandidateRequest) (orchestrator.CandidateEvidence, error) {
	id, patchPath, err := e.writeCandidatePatch(req)
	if err != nil {
		return orchestrator.CandidateEvidence{}, err
	}
	evidence := orchestrator.CandidateEvidence{
		Candidate:    domain.Candidate{ID: id, BaseRevision: req.Campaign.BaseRevision, Hypothesis: req.Proposal.Hypothesis, PatchPath: patchPath},
		ArtifactURIs: []string{patchPath},
	}
	prepared, ok := e.prepareCandidate(ctx, req, patchPath, &evidence)
	if !ok {
		return evidence, nil
	}
	defer func() { _ = prepared.Close(context.Background()) }()
	candidateBinary, ok := e.buildAndTestCandidate(ctx, prepared.Worktree, id, &evidence)
	if !ok {
		return evidence, nil
	}
	if !e.measureAndFinalize(ctx, &evidence, id, candidateBinary) {
		return evidence, nil
	}
	// Informational PGO lane: only candidates that passed the test suite and
	// produced a full ordinary verdict reach it, so the extra builds and A/B
	// series are never spent on rejected work.
	e.runPgoLane(ctx, &evidence, prepared.Worktree, id)
	return evidence, nil
}

func (e *Engine) writeCandidatePatch(req orchestrator.CandidateRequest) (id, patchPath string, err error) {
	patchDir := filepath.Join(e.dir, "patches")
	if err := os.MkdirAll(patchDir, 0o700); err != nil {
		return "", "", err
	}
	id = stableID("candidate", e.state.ID, fmt.Sprint(req.Attempt), req.Proposal.Patch)
	patchPath = filepath.Join(patchDir, id+".diff")
	if err := os.WriteFile(patchPath, []byte(req.Proposal.Patch), 0o600); err != nil {
		return "", "", err
	}
	return id, patchPath, nil
}

func (e *Engine) prepareCandidate(ctx context.Context, req orchestrator.CandidateRequest, patchPath string, evidence *orchestrator.CandidateEvidence) (*candidate.Prepared, bool) {
	prohibited := prohibitedTechniquesFor(e.state.Manifest.OptimizationPolicy)
	manager := &candidate.WorktreeManager{Toolchain: e.toolchain, Repository: e.state.Repository, Root: filepath.Join(e.dir, "worktrees")}
	prepared, err := manager.Prepare(ctx, req.Campaign.BaseRevision, patchPath, req.Proposal.Hypothesis, candidate.Policy{ProhibitedTechniques: prohibited})
	if err != nil {
		evidence.Summary = fmt.Sprintf("candidate rejected before build: %v", err)
		evidence.FailureDetail = tail(err.Error(), 400)
		return nil, false
	}
	evidence.Candidate = prepared.Candidate
	return prepared, true
}

func (e *Engine) buildAndTestCandidate(ctx context.Context, worktree, id string, evidence *orchestrator.CandidateEvidence) (string, bool) {
	binDir := filepath.Join(e.dir, "builds")
	candidateBinary := filepath.Join(binDir, id+"-"+filepath.Base(e.state.Manifest.Target.Build.Binary))
	if !e.buildCandidateBinary(ctx, worktree, candidateBinary, evidence) {
		return "", false
	}
	evidence.ArtifactURIs = append(evidence.ArtifactURIs, candidateBinary)
	if !e.candidateTestsPassed(ctx, worktree, evidence) {
		return "", false
	}
	return candidateBinary, true
}

func (e *Engine) buildCandidateBinary(ctx context.Context, worktree, candidateBinary string, evidence *orchestrator.CandidateEvidence) bool {
	buildResult, buildErr := e.toolchain.Build(ctx, toolchain.BuildRequest{Repository: worktree, Target: e.state.Manifest.Target.Build.Package, Output: candidateBinary, Env: []string{"GOTOOLCHAIN=local"}})
	if buildErr != nil {
		evidence.Summary = fmt.Sprintf("candidate build failed: %v", buildErr)
		evidence.FailureDetail = tail(string(buildResult.Stderr), 600)
		if evidence.FailureDetail == "" {
			evidence.FailureDetail = tail(buildErr.Error(), 600)
		}
		return false
	}
	return true
}

func (e *Engine) candidateTestsPassed(ctx context.Context, worktree string, evidence *orchestrator.CandidateEvidence) bool {
	// Behavior gate: the upstream test suite must pass on the patched tree.
	testResult, testErr := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: worktree, Env: []string{"GOTOOLCHAIN=local"}})
	if testErr == nil && testResult.ExitCode == 0 {
		return true
	}
	reason := "unknown failure"
	if testErr != nil {
		reason = testErr.Error()
	} else if len(testResult.Stderr) > 0 {
		reason = tail(string(testResult.Stderr), 400)
	}
	evidence.SafetyChecksPassed = false
	evidence.Summary = fmt.Sprintf("upstream test suite failed: %s", reason)
	evidence.FailureDetail = reason
	return false
}

type pooledSamples struct {
	wallBase, wallCand []float64
	cpuBase, cpuCand   []float64
	memBase, memCand   []float64
}

func (e *Engine) measureAndFinalize(ctx context.Context, evidence *orchestrator.CandidateEvidence, id, candidateBinary string) bool {
	baselineSize, candSize, sizeErr := binarySizes(e.state.BinaryPath, candidateBinary)
	comparisons := make([]domain.MetricComparison, 0, 4)
	var pooled pooledSamples
	measured, ok := e.measureSeedWorkloads(ctx, evidence, id, candidateBinary, &comparisons, &pooled)
	if !ok {
		return false
	}
	if measured == 0 {
		evidence.Summary = "no representative seed workloads available for measurement"
		evidence.Comparisons = comparisons
		return false
	}
	e.finalizeCandidateEvidence(ctx, evidence, comparisons, pooled, sizeErr, baselineSize, candSize, measured)
	return true
}

func (e *Engine) measureSeedWorkloads(ctx context.Context, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, comparisons *[]domain.MetricComparison, pooled *pooledSamples) (int, bool) {
	measured := 0
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		if !e.measureOneSeed(ctx, seed, id, candidateBinary, evidence, comparisons, pooled) {
			return measured, false
		}
		measured++
	}
	return measured, true
}

func (e *Engine) measureOneSeed(ctx context.Context, seed manifest.SeedWorkload, id, candidateBinary string, evidence *orchestrator.CandidateEvidence, comparisons *[]domain.MetricComparison, pooled *pooledSamples) bool {
	baseReq, wid := e.seedMeasurementRequest(seed, e.state.BuildID, e.state.BinaryPath)
	candReq := baseReq
	candReq.Build = runner.Build{ID: id, BinaryPath: candidateBinary}
	candReq.Workload.Command.Path = candidateBinary
	// First-execution warm-up: a freshly built binary pays a one-time OS
	// cost on its first exec (Gatekeeper scan, page-in) that otherwise
	// poisons repetition 0 of the candidate leg — observed as ~470ms vs
	// ~9ms steady state. Discarded, errors ignored.
	_, _ = e.runner.Run(ctx, candReq)
	deterministicOutput := e.outputIsDeterministic(ctx, baseReq)
	ab, err := e.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: measurementRepetitions})
	if err != nil {
		evidence.Summary = fmt.Sprintf("measurement failed on workload %q: %v", seed.ID, err)
		evidence.Comparisons = *comparisons
		return false
	}
	if !recordBehaviorMatch(ab, deterministicOutput, seed.ID, evidence, *comparisons) {
		return false
	}
	e.recordSeedMetrics(ctx, seed.ID, wid, ab, evidence, comparisons, pooled)
	return true
}

func (e *Engine) outputIsDeterministic(ctx context.Context, baseReq runner.RunRequest) bool {
	// Self-consistency probe: CLIs with nondeterministic tie ordering
	// (map iteration plus unstable sort) produce byte-different output
	// for identical inputs. Only when the baseline itself proves
	// deterministic do we hold the candidate to byte-exact equality;
	// otherwise the order-insensitive digest decides, so cosmetic row
	// order cannot reject a behavior-preserving patch.
	if probeA, err := e.runner.Run(ctx, baseReq); err == nil {
		if probeB, err := e.runner.Run(ctx, baseReq); err == nil && probeA.StdoutDigest != probeB.StdoutDigest {
			return false
		}
	}
	return true
}

func recordBehaviorMatch(ab runner.ABResult, deterministicOutput bool, seedID string, evidence *orchestrator.CandidateEvidence, comparisons []domain.MetricComparison) bool {
	for i := range ab.Baseline {
		exitOK := ab.Baseline[i].ExitCode == ab.Candidate[i].ExitCode
		stdoutOK := ab.Baseline[i].StdoutDigest == ab.Candidate[i].StdoutDigest
		if !deterministicOutput {
			stdoutOK = ab.Baseline[i].SortedLinesDigest == ab.Candidate[i].SortedLinesDigest && ab.Baseline[i].SortedLinesDigest != ""
		}
		if stdoutOK && exitOK {
			continue
		}
		basis := "byte-exact"
		if !deterministicOutput {
			basis = "order-insensitive"
		}
		evidence.Summary = fmt.Sprintf("behavior mismatch (%s comparison) on workload %q at repetition %d", basis, seedID, i+1)
		evidence.Comparisons = comparisons
		evidence.SafetyChecksPassed = true
		return false
	}
	return true
}

func (e *Engine) recordSeedMetrics(ctx context.Context, seedID, wid string, ab runner.ABResult, evidence *orchestrator.CandidateEvidence, comparisons *[]domain.MetricComparison, pooled *pooledSamples) {
	wallComparisons, wallBenchstat := e.compareWallTimeMetric(ctx, wid, ab.Baseline, ab.Candidate)
	*comparisons = append(*comparisons, wallComparisons...)
	baseSamples := metricValues(ab.Baseline, wallTime)
	candSamples := metricValues(ab.Candidate, wallTime)
	evidence.RepSamples = append(evidence.RepSamples, domain.WorkloadSamples{WorkloadID: wid, BaselineNs: baseSamples, CandidateNs: candSamples})
	if wallBenchstat != "" {
		if evidence.BenchstatOutput != "" {
			evidence.BenchstatOutput += "\n\n"
		}
		evidence.BenchstatOutput += fmt.Sprintf("workload %s:\n%s", seedID, wallBenchstat)
	}
	pooled.cpuBase = append(pooled.cpuBase, metricValues(ab.Baseline, cpuTime)...)
	pooled.memBase = append(pooled.memBase, metricValues(ab.Baseline, peakMemory)...)
	pooled.cpuCand = append(pooled.cpuCand, metricValues(ab.Candidate, cpuTime)...)
	pooled.memCand = append(pooled.memCand, metricValues(ab.Candidate, peakMemory)...)
	pooled.wallBase = append(pooled.wallBase, baseSamples...)
	pooled.wallCand = append(pooled.wallCand, candSamples...)
}

func metricValues(runs []domain.RunResult, sel metricSelector) []float64 {
	var values []float64
	for _, r := range runs {
		if v, ok := sel(r); ok {
			values = append(values, v)
		}
	}
	return values
}

func (e *Engine) finalizeCandidateEvidence(ctx context.Context, evidence *orchestrator.CandidateEvidence, comparisons []domain.MetricComparison, pooled pooledSamples, sizeErr error, baselineSize, candSize int64, measured int) {
	// Policy looks up canonical metric names ("wall_time_ns" primary plus
	// required guardrails), so samples from all representative workloads
	// are pooled into one comparison per metric. Per-workload raw data
	// remains available in RepSamples for diagnosis.
	wallComparisons, wallBenchstat := e.compareWallTimeMetric(ctx, "", pooledAsRuns(pooled.wallBase, "wall_time_ns"), pooledAsRuns(pooled.wallCand, "wall_time_ns"))
	comparisons = append(comparisons, wallComparisons...)
	if wallBenchstat != "" {
		evidence.BenchstatOutput += fmt.Sprintf("pooled representative workloads:\n%s", wallBenchstat)
	}
	comparisons = append(comparisons, compareMetric("", "cpu_time_ns", "ns",
		pooledAsRuns(pooled.cpuBase, "cpu_time_ns"), pooledAsRuns(pooled.cpuCand, "cpu_time_ns"), cpuTime)...)
	comparisons = append(comparisons, compareMetric("", "peak_memory_bytes", "bytes",
		pooledAsRuns(pooled.memBase, "peak_memory_bytes"), pooledAsRuns(pooled.memCand, "peak_memory_bytes"), peakMemory)...)
	if sizeErr == nil {
		comparisons = append(comparisons, domain.MetricComparison{Name: "binary_size_bytes", Unit: "bytes", Baseline: float64(baselineSize), Candidate: float64(candSize), DeltaPercent: percentDelta(float64(baselineSize), float64(candSize)), StatisticallyFit: true})
	}
	evidence.BehaviorMatches = true
	evidence.SafetyChecksPassed = true
	evidence.RepresentativeEvidence = measured > 0
	evidence.Comparisons = comparisons
	evidence.ValidationJobs = append(evidence.ValidationJobs, "build", "test-suite", "interleaved-ab")
	evidence.Summary = fmt.Sprintf("patched tree built; tests passed; %d representative workload(s) measured over %d A/B pairs each", measured, measurementRepetitions)
}

// seedMeasurementRequest builds the measurement runner request for one seed
// workload against a specific binary. The workload ID is deterministic per
// campaign and seed so baseline and variant runs share it.
func (e *Engine) seedMeasurementRequest(seed manifest.SeedWorkload, buildID, binaryPath string) (runner.RunRequest, string) {
	fixtures := make(map[string][]byte, len(seed.Files))
	for _, f := range seed.Files {
		fixtures[f.Path] = []byte(f.Content)
	}
	args := append(append([]string{}, e.state.Manifest.Target.Command...), seed.Args...)
	timeout := seed.Timeout.Duration()
	if timeout == 0 {
		timeout = e.state.Manifest.Campaign.MinimumCommandTimeout.Duration()
	}
	wid := stableID("workload", e.state.ID, seed.ID)
	req := runner.RunRequest{
		Build:         runner.Build{ID: buildID, BinaryPath: binaryPath},
		Workload:      domain.Workload{ID: wid, Name: seed.Name, Tier: seed.Tier, Command: domain.Command{Path: binaryPath, Args: args}, Timeout: timeout},
		Mode:          domain.RunModeMeasurement,
		Stdin:         []byte(seed.Stdin),
		Fixtures:      fixtures,
		AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"},
	}
	return req, wid
}

// runPgoLane is the informational profile-guided-optimization lane. It never
// changes accept/reject decisions: policy judges only evidence.Comparisons;
// PgoComparisons/PgoNote exist purely to attribute how much of an accepted
// patch's effect comes from the compiler's -pgo feedback versus the source
// change itself.
//
// Trigger conditions: the candidate passed the upstream test suite and the
// ordinary interleaved A/B series completed, AND discovery produced a raw
// pprof-format CPU profile (benchmark-based collection). Sampler reports are
// not pprof and cannot seed -pgo, so their presence skips this lane with a
// recorded reason. The same discovery profile is reused; nothing is
// re-collected, keeping total runtime bounded at one extra build pair plus
// one 7-repetition interleaved series per representative workload.
func (e *Engine) runPgoLane(ctx context.Context, evidence *orchestrator.CandidateEvidence, candidateWorktree, candidateID string) {
	if ok, reason := e.pgoProfileReady(); !ok {
		e.skipPgoLane(evidence, reason)
		return
	}
	baselinePgo, candidatePgo, ok := e.buildPgoBinaries(ctx, candidateWorktree, candidateID, evidence)
	if !ok {
		return
	}
	comparisons, measured, ok := e.measurePgoWorkloads(ctx, evidence, candidateID, baselinePgo, candidatePgo)
	if !ok {
		return
	}
	if measured == 0 {
		e.skipPgoLane(evidence, "no representative seed workloads available for measurement")
		return
	}
	evidence.PgoComparisons = comparisons
	evidence.PgoNote = fmt.Sprintf("informational PGO comparison over %d representative workload(s), %d A/B pairs each; both sides built with -pgo=%s from the discovery CPU profile; this lane never changes accept/reject decisions", measured, measurementRepetitions, filepath.Base(e.state.PGOProfilePath))
	_ = e.saveEvent("pgo_lane_completed", evidence.PgoNote, nil)
}

func (e *Engine) skipPgoLane(evidence *orchestrator.CandidateEvidence, reason string) {
	evidence.PgoNote = "PGO lane skipped: " + reason
	_ = e.saveEvent("pgo_lane_skipped", evidence.PgoNote, nil)
}

func (e *Engine) pgoProfileReady() (bool, string) {
	if e.state.PGOProfilePath == "" {
		return false, "no pprof-format CPU profile was collected during discovery (sampler reports are not pprof and cannot seed -pgo)"
	}
	if info, err := os.Stat(e.state.PGOProfilePath); err != nil || info.IsDir() || info.Size() == 0 {
		return false, fmt.Sprintf("discovery CPU profile %q missing or empty on disk", e.state.PGOProfilePath)
	}
	return true, ""
}

func (e *Engine) buildPgoBinaries(ctx context.Context, candidateWorktree, candidateID string, evidence *orchestrator.CandidateEvidence) (baselinePgo, candidatePgo string, ok bool) {
	binDir := filepath.Join(e.dir, "builds")
	binarySuffix := filepath.Base(e.state.Manifest.Target.Build.Binary)
	baselinePgo = filepath.Join(binDir, candidateID+"-baseline-pgo-"+binarySuffix)
	candidatePgo = filepath.Join(binDir, candidateID+"-candidate-pgo-"+binarySuffix)
	for _, target := range []struct {
		label, repository, output string
	}{
		{"baseline-pgo", e.state.Repository, baselinePgo},
		{"candidate-pgo", candidateWorktree, candidatePgo},
	} {
		if !e.buildPgoBinary(ctx, target.label, target.repository, target.output, evidence) {
			return "", "", false
		}
	}
	return baselinePgo, candidatePgo, true
}

func (e *Engine) buildPgoBinary(ctx context.Context, label, repository, output string, evidence *orchestrator.CandidateEvidence) bool {
	result, err := e.toolchain.Build(ctx, toolchain.BuildRequest{Repository: repository, Target: e.state.Manifest.Target.Build.Package, Output: output, PGOProfile: e.state.PGOProfilePath, Env: []string{"GOTOOLCHAIN=local"}})
	if err != nil {
		detail := tail(string(result.Stderr), 200)
		if detail == "" {
			detail = err.Error()
		}
		e.skipPgoLane(evidence, fmt.Sprintf("%s build failed: %s", label, detail))
		return false
	}
	evidence.ArtifactURIs = append(evidence.ArtifactURIs, output)
	return true
}

func (e *Engine) measurePgoWorkloads(ctx context.Context, evidence *orchestrator.CandidateEvidence, candidateID, baselinePgo, candidatePgo string) ([]domain.MetricComparison, int, bool) {
	comparisons := make([]domain.MetricComparison, 0, 3)
	measured := 0
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		baseReq, wid := e.seedMeasurementRequest(seed, candidateID+"-baseline-pgo", baselinePgo)
		candReq := baseReq
		candReq.Build = runner.Build{ID: candidateID + "-candidate-pgo", BinaryPath: candidatePgo}
		candReq.Workload.Command.Path = candidatePgo
		ab, err := e.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: measurementRepetitions})
		if err != nil {
			e.skipPgoLane(evidence, fmt.Sprintf("measurement failed on workload %q: %v", seed.ID, err))
			return nil, 0, false
		}
		if ok, rep := pgoBehaviorOK(ab); !ok {
			e.skipPgoLane(evidence, fmt.Sprintf("PGO-built binaries diverged on workload %q at repetition %d", seed.ID, rep))
			return nil, 0, false
		}
		comparisons = append(comparisons, compareMetric(wid, "wall_time_ns", "ns", ab.Baseline, ab.Candidate, wallTime)...)
		comparisons = append(comparisons, compareMetric(wid, "cpu_time_ns", "ns", ab.Baseline, ab.Candidate, cpuTime)...)
		comparisons = append(comparisons, compareMetric(wid, "peak_memory_bytes", "bytes", ab.Baseline, ab.Candidate, peakMemory)...)
		measured++
	}
	return comparisons, measured, true
}

func pgoBehaviorOK(ab runner.ABResult) (bool, int) {
	// Informational behavior sanity check: tolerate output-order noise
	// without paying for the determinism probe, since a divergence here
	// only invalidates the comparison, never the candidate verdict.
	for i := range ab.Baseline {
		exitOK := ab.Baseline[i].ExitCode == ab.Candidate[i].ExitCode
		stdoutOK := ab.Baseline[i].StdoutDigest == ab.Candidate[i].StdoutDigest ||
			(ab.Baseline[i].SortedLinesDigest != "" && ab.Baseline[i].SortedLinesDigest == ab.Candidate[i].SortedLinesDigest)
		if !exitOK || !stdoutOK {
			return false, i + 1
		}
	}
	return true, 0
}

// pooledAsRuns adapts raw sample values into single-metric RunResults so
// compareMetric can consume pooled cross-workload samples.
func pooledAsRuns(values []float64, metric string) []domain.RunResult {
	runs := make([]domain.RunResult, 0, len(values))
	for _, v := range values {
		runs = append(runs, domain.RunResult{Metrics: []domain.Metric{{Name: metric, Value: v}}})
	}
	return runs
}

type metricSelector func(domain.RunResult) (float64, bool)

func wallTime(r domain.RunResult) (float64, bool) {
	for _, m := range r.Metrics {
		if m.Name == "wall_time_ns" {
			return m.Value, true
		}
	}
	return 0, false
}

func cpuTime(r domain.RunResult) (float64, bool) {
	for _, m := range r.Metrics {
		if m.Name == "cpu_time_ns" {
			return m.Value, true
		}
	}
	return 0, false
}

func peakMemory(r domain.RunResult) (float64, bool) {
	for _, m := range r.Metrics {
		if m.Name == "peak_memory_bytes" {
			return m.Value, true
		}
	}
	return 0, false
}

// compareMetric aggregates one metric across A/B samples into a comparison
// with a conservative two-sample t-test for statistical support. The
// comparison name carries the workload so guardrails can be evaluated per
// workload; policy treats any regressed guardrail as a rejection.
func compareMetric(workloadID, metric, unit string, baseline, candidateRuns []domain.RunResult, select_ metricSelector) []domain.MetricComparison {
	baseVals := collectMetric(baseline, select_)
	candVals := collectMetric(candidateRuns, select_)
	name := metric
	if workloadID != "" {
		name = workloadID + "/" + metric
	}
	result := domain.MetricComparison{Name: name, Unit: unit}
	meanBase, okBase := mean(baseVals)
	meanCand, okCand := mean(candVals)
	if !okBase || !okCand {
		return []domain.MetricComparison{result}
	}
	result.Baseline = meanBase
	result.Candidate = meanCand
	if meanBase > 0 {
		result.DeltaPercent = percentDelta(meanBase, meanCand)
	}
	result.StatisticallyFit = metricSupport(baseVals, candVals, result.Baseline, result.DeltaPercent)
	return []domain.MetricComparison{result}
}

func collectMetric(runs []domain.RunResult, s metricSelector) []float64 {
	values := make([]float64, 0, len(runs))
	for _, r := range runs {
		if v, ok := s(r); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			values = append(values, v)
		}
	}
	return values
}

func mean(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values)), true
}

// metricSupport answers the question policy actually asks: "are these
// samples strong evidence about the candidate's effect?" A comparison is
// supported when the Welch test detects a significant difference OR when
// the 95% confidence interval of the relative delta lies entirely below
// the 2% guardrail limit, i.e. the data confidently rules out a material
// regression even though no significant change was detected. The latter
// case matters for jittery metrics like peak memory, where a genuinely
// flat candidate otherwise reads as unsupported.
func metricSupport(a, b []float64, baselineMean, deltaPercent float64) bool {
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	ma, ok := mean(a)
	if !ok {
		return false
	}
	mb, _ := mean(b)
	va, vb := variance(a), variance(b)
	se := math.Sqrt(va/float64(len(a)) + vb/float64(len(b)))
	if se == 0 {
		return ma == mb // exact identical measurements
	}
	t := math.Abs(ma - mb) / se
	if t > 2.2 {
		return true
	}
	if baselineMean <= 0 || deltaPercent >= 2.0 {
		return false
	}
	ciUpper := ((mb - ma) + 2.2*se) / baselineMean * 100
	return ciUpper < 2.0
}

// statisticallySupported is retained for direct significance queries.
func statisticallySupported(a, b []float64) bool {
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	ma, ok := mean(a)
	if !ok {
		return false
	}
	mb, _ := mean(b)
	va, vb := variance(a), variance(b)
	se2 := va/float64(len(a)) + vb/float64(len(b))
	if se2 <= 0 {
		// Identical constant measurements: no detectable difference.
		return ma != mb
	}
	t := math.Abs(ma-mb) / math.Sqrt(se2)
	return t > 2.2
}

func variance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	m, _ := mean(values)
	sum := 0.0
	for _, v := range values {
		sum += (v - m) * (v - m)
	}
	return sum / float64(len(values)-1)
}

func percentDelta(baseline, candidate float64) float64 {
	if baseline == 0 {
		return 0
	}
	return (candidate - baseline) / baseline * 100
}

func binarySizes(baselinePath, candidatePath string) (int64, int64, error) {
	baseInfo, err := os.Stat(baselinePath)
	if err != nil {
		return 0, 0, err
	}
	candInfo, err := os.Stat(candidatePath)
	if err != nil {
		return 0, 0, err
	}
	if baseInfo.Size() <= 0 || candInfo.Size() <= 0 {
		return 0, 0, errors.New("binary size must be positive")
	}
	return baseInfo.Size(), candInfo.Size(), nil
}

func tail(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}

func prohibitedTechniquesFor(mode domain.OptimizationPolicy) []string {
	switch mode {
	case domain.PolicySpecialized:
		return []string{"unsafe.", "assembly", "cgo"}
	case domain.PolicyNative:
		return []string{"cgo"}
	default:
		return nil
	}
}
