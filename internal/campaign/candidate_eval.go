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
	patchDir := filepath.Join(e.dir, "patches")
	if err := os.MkdirAll(patchDir, 0o700); err != nil {
		return orchestrator.CandidateEvidence{}, err
	}
	id := stableID("candidate", e.state.ID, fmt.Sprint(req.Attempt), req.Proposal.Patch)
	patchPath := filepath.Join(patchDir, id+".diff")
	if err := os.WriteFile(patchPath, []byte(req.Proposal.Patch), 0o600); err != nil {
		return orchestrator.CandidateEvidence{}, err
	}

	evidence := orchestrator.CandidateEvidence{
		Candidate:    domain.Candidate{ID: id, BaseRevision: req.Campaign.BaseRevision, Hypothesis: req.Proposal.Hypothesis, PatchPath: patchPath},
		ArtifactURIs: []string{patchPath},
	}

	prohibited := prohibitedTechniquesFor(e.state.Manifest.OptimizationPolicy)
	manager := &candidate.WorktreeManager{Toolchain: e.toolchain, Repository: e.state.Repository, Root: filepath.Join(e.dir, "worktrees")}
	prepared, err := manager.Prepare(ctx, req.Campaign.BaseRevision, patchPath, req.Proposal.Hypothesis, candidate.Policy{ProhibitedTechniques: prohibited})
	if err != nil {
		evidence.Summary = fmt.Sprintf("candidate rejected before build: %v", err)
		evidence.FailureDetail = tail(err.Error(), 400)
		return evidence, nil
	}
	defer func() { _ = prepared.Close(context.Background()) }()
	evidence.Candidate = prepared.Candidate

	binDir := filepath.Join(e.dir, "builds")
	candidateBinary := filepath.Join(binDir, id+"-"+filepath.Base(e.state.Manifest.Target.Build.Binary))
	buildResult, buildErr := e.toolchain.Build(ctx, toolchain.BuildRequest{Repository: prepared.Worktree, Target: e.state.Manifest.Target.Build.Package, Output: candidateBinary, Env: []string{"GOTOOLCHAIN=local"}})
	if buildErr != nil {
		evidence.Summary = fmt.Sprintf("candidate build failed: %v", buildErr)
		evidence.FailureDetail = tail(string(buildResult.Stderr), 600)
		if evidence.FailureDetail == "" {
			evidence.FailureDetail = tail(buildErr.Error(), 600)
		}
		return evidence, nil
	}
	evidence.ArtifactURIs = append(evidence.ArtifactURIs, candidateBinary)

	// Behavior gate: the upstream test suite must pass on the patched tree.
	testResult, testErr := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: prepared.Worktree, Env: []string{"GOTOOLCHAIN=local"}})
	testsPassed := testErr == nil && testResult.ExitCode == 0
	if !testsPassed {
		reason := "unknown failure"
		if testErr != nil {
			reason = testErr.Error()
		} else if len(testResult.Stderr) > 0 {
			reason = tail(string(testResult.Stderr), 400)
		}
		evidence.SafetyChecksPassed = false
		evidence.Summary = fmt.Sprintf("upstream test suite failed: %s", reason)
		evidence.FailureDetail = reason
		return evidence, nil
	}

	baselineSize, candSize, sizeErr := binarySizes(e.state.BinaryPath, candidateBinary)
	comparisons := make([]domain.MetricComparison, 0, 4)
	behaviorMatches := true
	measuredWorkloads := 0

	for _, seed := range e.state.Manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
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
		baseReq := runner.RunRequest{
			Build:         runner.Build{ID: e.state.BuildID, BinaryPath: e.state.BinaryPath},
			Workload:      domain.Workload{ID: wid, Name: seed.Name, Tier: seed.Tier, Command: domain.Command{Path: e.state.BinaryPath, Args: args}, Timeout: timeout},
			Mode:          domain.RunModeMeasurement,
			Stdin:         []byte(seed.Stdin),
			Fixtures:      fixtures,
			AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"},
		}
		candReq := baseReq
		candReq.Build = runner.Build{ID: id, BinaryPath: candidateBinary}
		candReq.Workload.Command.Path = candidateBinary

		// Self-consistency probe: CLIs with nondeterministic tie ordering
		// (map iteration plus unstable sort) produce byte-different output
		// for identical inputs. Only when the baseline itself proves
		// deterministic do we hold the candidate to byte-exact equality;
		// otherwise the order-insensitive digest decides, so cosmetic row
		// order cannot reject a behavior-preserving patch.
		deterministicOutput := true
		if probeA, err := e.runner.Run(ctx, baseReq); err == nil {
			if probeB, err := e.runner.Run(ctx, baseReq); err == nil && probeA.StdoutDigest != probeB.StdoutDigest {
				deterministicOutput = false
			}
		}

		ab, err := e.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: measurementRepetitions})
		if err != nil {
			evidence.Summary = fmt.Sprintf("measurement failed on workload %q: %v", seed.ID, err)
			evidence.Comparisons = comparisons
			return evidence, nil
		}
		for i := range ab.Baseline {
			exitOK := ab.Baseline[i].ExitCode == ab.Candidate[i].ExitCode
			stdoutOK := ab.Baseline[i].StdoutDigest == ab.Candidate[i].StdoutDigest
			if !deterministicOutput {
				stdoutOK = ab.Baseline[i].SortedLinesDigest == ab.Candidate[i].SortedLinesDigest && ab.Baseline[i].SortedLinesDigest != ""
			}
			if !stdoutOK || !exitOK {
				behaviorMatches = false
				basis := "byte-exact"
				if !deterministicOutput {
					basis = "order-insensitive"
				}
				evidence.Summary = fmt.Sprintf("behavior mismatch (%s comparison) on workload %q at repetition %d", basis, seed.ID, i+1)
				evidence.Comparisons = comparisons
				evidence.SafetyChecksPassed = testsPassed
				return evidence, nil
			}
		}
		wallComparisons, wallBenchstat := e.compareWallTimeMetric(ctx, wid, ab.Baseline, ab.Candidate)
		comparisons = append(comparisons, wallComparisons...)
		if wallBenchstat != "" {
			if evidence.BenchstatOutput != "" {
				evidence.BenchstatOutput += "\n\n"
			}
			evidence.BenchstatOutput += fmt.Sprintf("workload %s:\n%s", seed.ID, wallBenchstat)
		}
		comparisons = append(comparisons, compareMetric(wid, "cpu_time_ns", "ns", ab.Baseline, ab.Candidate, cpuTime)...) 
		comparisons = append(comparisons, compareMetric(wid, "peak_memory_bytes", "bytes", ab.Baseline, ab.Candidate, peakMemory)...)
		measuredWorkloads++
	}

	if measuredWorkloads == 0 {
		evidence.Summary = "no representative seed workloads available for measurement"
		evidence.Comparisons = comparisons
		return evidence, nil
	}

	if sizeErr == nil {
		comparisons = append(comparisons, domain.MetricComparison{Name: "binary_size_bytes", Unit: "bytes", Baseline: float64(baselineSize), Candidate: float64(candSize), DeltaPercent: percentDelta(float64(baselineSize), float64(candSize)), StatisticallyFit: true})
	}

	evidence.BehaviorMatches = behaviorMatches
	evidence.SafetyChecksPassed = testsPassed
	evidence.RepresentativeEvidence = measuredWorkloads > 0
	evidence.Comparisons = comparisons
	evidence.ValidationJobs = append(evidence.ValidationJobs, "build", "test-suite", "interleaved-ab")
	evidence.Summary = fmt.Sprintf("patched tree built; tests passed; %d representative workload(s) measured over %d A/B pairs each", measuredWorkloads, measurementRepetitions)
	return evidence, nil
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
	result := domain.MetricComparison{Name: workloadID + "/" + metric, Unit: unit}
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
	result.StatisticallyFit = statisticallySupported(baseVals, candVals)
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

// statisticallySupported applies a two-sample Welch t-test against a
// conservative critical value (|t| > 2.2, roughly p < 0.05 for the small
// equal-n samples this harness produces). It never reports support from
// fewer than four samples per side.
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
