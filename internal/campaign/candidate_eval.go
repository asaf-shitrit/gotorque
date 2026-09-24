package campaign

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/candidate"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/policy"
	"example.com/gotorque/internal/runner"
	"example.com/gotorque/internal/toolchain"
)

// measurementRepetitions is the interleaved A/B sample count per workload.
//
// Twenty-five pairs, not seven. The acceptance bar is 3% of wall time, and a
// short CLI workload carries roughly 6-8% per-run spread (gojq ~9ms runs,
// gron's 84 KiB seed ~23ms), so seven pairs leave a 3% effect sitting near
// 1.5σ — inside the noise the Welch t-test is asked to see through. Measured
// directly on a gron candidate that the campaign recorded as inconclusive at
// -3.51%: thirty interleaved pairs put its real effect at -3.75% with p<1e-4,
// while the campaign's seven pairs could not resolve it. The extra runs cost
// about a second per workload, against minutes for the model call and the
// build that produced the candidate.
const measurementRepetitions = 25

// The PGO lane is informational and never changes a verdict, so it must not be
// able to spend the campaign's budget. A target whose profile-guided build is
// slower than pgoLaneBuildTimeout gets a recorded skip instead of an eaten
// run: on gron one `-pgo` build ran for thirty-six minutes inside a
// forty-minute campaign, the deadline fired, and the candidate's completed
// measurement was never recorded — the whole campaign ended with no verdict at
// all. The floor keeps a lane that is about to be cut short from starting in
// the tail of a campaign.
const (
	pgoLaneBuildTimeout = 5 * time.Minute
)

// evaluateCandidate runs the deterministic half of the candidate loop:
// validate the proposed diff, apply it in an isolated worktree, build,
// gate on the upstream test suite, then measure baseline and candidate
// binaries interleaved on representative seed workloads. Every terminal
// judgment stays here or in policy; the model never self-approves.
// evaluateCandidate is invoked through the orchestrator CandidateService adapter.
func (e *Engine) evaluateCandidate(ctx context.Context, req orchestrator.CandidateRequest) (orchestrator.CandidateEvidence, error) {
	patchText, transport, err := e.resolveCandidatePatch(ctx, req)
	if err != nil {
		// Building the diff from function_source failed before there was
		// anything to write or apply: a pre-build rejection like a patch that
		// does not parse, marked unmeasured so the target is offered once
		// more (ADR 0017).
		return orchestrator.CandidateEvidence{
			Candidate:     domain.Candidate{BaseRevision: req.Campaign.BaseRevision, Hypothesis: req.Proposal.Hypothesis, Transport: transport},
			Summary:       fmt.Sprintf("candidate rejected before build: %v", err),
			FailureDetail: tail(err.Error(), 400),
			Unmeasured:    true,
		}, nil
	}
	id, patchPath, err := e.writeCandidatePatch(req, patchText)
	if err != nil {
		return orchestrator.CandidateEvidence{}, err
	}
	evidence := orchestrator.CandidateEvidence{
		Candidate:    domain.Candidate{ID: id, BaseRevision: req.Campaign.BaseRevision, Hypothesis: req.Proposal.Hypothesis, PatchPath: patchPath, Transport: transport},
		ArtifactURIs: []string{patchPath},
	}
	prepared, ok := e.prepareCandidate(ctx, req, patchPath, &evidence)
	if !ok {
		return evidence, nil
	}
	// prepareCandidate replaces evidence.Candidate wholesale with the one
	// WorktreeManager.Prepare built, which does not know the transport.
	evidence.Candidate.Transport = transport
	// Worktree teardown runs even when the caller's context is already
	// canceled (duration budget or Ctrl-C), but keeps its values.
	defer func() { _ = prepared.Close(context.WithoutCancel(ctx)) }()
	if !e.patchHasShape(ctx, prepared.Worktree, req.Target, &evidence) {
		return evidence, nil
	}
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

func (e *Engine) writeCandidatePatch(req orchestrator.CandidateRequest, patchText string) (id, patchPath string, err error) {
	patchDir := filepath.Join(e.dir, "patches")
	if err := os.MkdirAll(patchDir, 0o700); err != nil {
		return "", "", err
	}
	id = stableID("candidate", e.state.ID, strconv.Itoa(req.Attempt), patchText)
	patchPath = filepath.Join(patchDir, id+".diff")
	if err := os.WriteFile(patchPath, []byte(patchText), 0o600); err != nil {
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
		evidence.Unmeasured = true
		return nil, false
	}
	evidence.Candidate = prepared.Candidate
	return prepared, true
}

// patchHasShape runs checkShape on the applied worktree. A diff Git cannot
// produce leaves the judgment to the build, which would fail on the same
// tree; the check only ever adds a reason, never a pass.
func (e *Engine) patchHasShape(ctx context.Context, worktree string, target *agents.Target, evidence *orchestrator.CandidateEvidence) bool {
	diff, err := e.toolchain.ChangedLines(ctx, worktree)
	if err != nil {
		return true
	}
	if err := checkShape(worktree, diff, target); err != nil {
		evidence.Summary = fmt.Sprintf("candidate rejected before build: patch shape: %v", err)
		evidence.FailureDetail = tail(err.Error(), 400)
		evidence.Unmeasured = true
		return false
	}
	return true
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
		evidence.Unmeasured = true
		evidence.FailureDetail = tail(string(buildResult.Stderr), 600)
		if evidence.FailureDetail == "" {
			evidence.FailureDetail = tail(buildErr.Error(), 600)
		}
		return false
	}
	return true
}

func (e *Engine) candidateTestsPassed(ctx context.Context, worktree string, evidence *orchestrator.CandidateEvidence) bool {
	// Behavior gate: the upstream test suite must not regress against the
	// unpatched revision. Failures that predate the patch are subtracted
	// rather than charged to it. A clean exit is classified too: it is what a
	// suite reports after a test the baseline passed was skipped or dropped.
	testResult, testErr := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: worktree, JSON: true, Env: []string{"GOTOOLCHAIN=local"}})
	reason, passed := e.classifyTestOutcome(testResult, testErr)
	if passed {
		return true
	}
	evidence.SafetyChecksPassed = false
	evidence.Summary = "upstream test suite failed: " + reason
	evidence.FailureDetail = reason
	return false
}

// pooledSamples keeps one sample series per representative workload rather
// than one flat series.
//
// Concatenating raw series from workloads of different scale inflates the
// pooled spread with between-workload variance, and Welch's t-test then
// measures the workload mix instead of the candidate's effect. On gron the two
// representative workloads average 23.6ms and 10.5ms, so a real 20.8% win on
// the large one produced a pooled t of 1.06 against 10.69 measured on the
// affected workload alone; the candidate was reported inconclusive. Folding
// across workloads per repetition keeps the test on the effect.
type pooledSamples struct {
	wallBase, wallCand [][]float64
	cpuBase, cpuCand   [][]float64
	memBase, memCand   [][]float64
}

// measurement is what a candidate's interleaved series produce. The raw runs
// are kept per seed so a confirmation series can extend them and every
// comparison can be derived again from both.
type measurement struct {
	seeds                  []seedRuns
	comparisons            []domain.MetricComparison
	pooled                 pooledSamples
	sizeErr                error
	baselineSize, candSize int64
}

// seedRuns is one representative seed's interleaved runs so far.
type seedRuns struct {
	seed          manifest.SeedWorkload
	deterministic bool
	ab            runner.ABResult
}

func (e *Engine) measureAndFinalize(ctx context.Context, evidence *orchestrator.CandidateEvidence, id, candidateBinary string) bool {
	m := measurement{comparisons: make([]domain.MetricComparison, 0, 4)}
	m.baselineSize, m.candSize, m.sizeErr = binarySizes(e.state.BinaryPath, candidateBinary)
	if !e.measureSeedWorkloads(ctx, evidence, id, candidateBinary, &m) {
		return false
	}
	if len(m.seeds) == 0 {
		evidence.Summary = "no representative seed workloads available for measurement"
		evidence.Comparisons = m.comparisons
		return false
	}
	e.finalizeCandidateEvidence(ctx, evidence, &m)
	evidence.ValidationJobs = append(evidence.ValidationJobs, "build", "test-suite", "interleaved-ab")
	return e.confirmRegressions(ctx, evidence, id, candidateBinary, &m)
}

func (e *Engine) measureSeedWorkloads(ctx context.Context, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		if !e.measureOneSeed(ctx, seed, id, candidateBinary, evidence, m) {
			return false
		}
	}
	return true
}

func (e *Engine) measureOneSeed(ctx context.Context, seed manifest.SeedWorkload, id, candidateBinary string, evidence *orchestrator.CandidateEvidence, m *measurement) bool {
	baseReq, candReq := e.abRequests(seed, id, candidateBinary)
	// First-execution warm-up: a freshly built binary pays a one-time OS
	// cost on its first exec (Gatekeeper scan, page-in) that otherwise
	// poisons repetition 0 of the candidate leg — observed as ~470ms vs
	// ~9ms steady state. Discarded, errors ignored.
	_, _ = e.runner.Run(ctx, candReq)
	runs := seedRuns{seed: seed, deterministic: e.outputIsDeterministic(ctx, baseReq)}
	if !e.runSeries(ctx, &runs, id, candidateBinary, evidence, m.comparisons) {
		return false
	}
	m.seeds = append(m.seeds, runs)
	e.recordSeedMetrics(ctx, seed.ID, runs.ab, evidence, m)
	return true
}

func (e *Engine) abRequests(seed manifest.SeedWorkload, id, candidateBinary string) (runner.RunRequest, runner.RunRequest) {
	baseReq := e.seedMeasurementRequest(seed, e.state.BuildID, e.state.BinaryPath)
	candReq := baseReq
	candReq.Build = runner.Build{ID: id, BinaryPath: candidateBinary}
	candReq.Workload.Command.Path = candidateBinary
	return baseReq, candReq
}

// runSeries runs one series of interleaved pairs on a seed, holds it to the
// seed's behaviour, and appends its runs to the ones already measured. A
// series that fails leaves the behaviour unverified even when an earlier one
// passed, so a confirmation series that fails rejects the candidate as a first
// series would.
func (e *Engine) runSeries(ctx context.Context, runs *seedRuns, id, candidateBinary string, evidence *orchestrator.CandidateEvidence, comparisons []domain.MetricComparison) bool {
	baseReq, candReq := e.abRequests(runs.seed, id, candidateBinary)
	ab, err := e.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: measurementRepetitions})
	if err != nil {
		evidence.BehaviorMatches = false
		evidence.Summary = fmt.Sprintf("measurement failed on workload %q: %v", runs.seed.ID, err)
		evidence.Comparisons = comparisons
		return false
	}
	if !recordBehaviorMatch(ab, runs.deterministic, runs.seed.ID, evidence, comparisons) {
		evidence.BehaviorMatches = false
		return false
	}
	runs.ab.Baseline = append(runs.ab.Baseline, ab.Baseline...)
	runs.ab.Candidate = append(runs.ab.Candidate, ab.Candidate...)
	return true
}

// confirmRegressions measures every representative seed again when an
// eligible reading regressed past the limit without significance, then derives
// every comparison from both series. The policy does not reject on such a
// reading, so without more samples a real regression that twenty-five pairs
// could not resolve would pass unnoticed. With them it either becomes
// significant and rejects, or stays insignificant and is named in the
// verdict's reasons. Every seed is extended, not only the one that read high,
// because the pooled reading folds the seeds per repetition and needs series
// of one length.
//
// The second series is part of measurement, so it has no budget of its own:
// it costs what the first did, seconds for a short CLI, and the campaign's
// deadline bounds it as it bounds the first.
func (e *Engine) confirmRegressions(ctx context.Context, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	config := policyConfigFromManifest(e.state.Manifest)
	unconfirmed := policy.UnconfirmedRegressions(config, eligibleReadings(config, evidence.Comparisons))
	if len(unconfirmed) == 0 {
		return true
	}
	for i := range m.seeds {
		if !e.runSeries(ctx, &m.seeds[i], id, candidateBinary, evidence, evidence.Comparisons) {
			return false
		}
	}
	e.rederive(ctx, evidence, m)
	note := confirmationNote(unconfirmed, config.MaximumGuardrailRegressionPercent)
	evidence.Summary += "; " + note
	evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	_ = e.saveEvent("measurement_confirmed", note, nil)
	return true
}

// rederive computes every comparison again from the runs measured so far.
func (e *Engine) rederive(ctx context.Context, evidence *orchestrator.CandidateEvidence, m *measurement) {
	evidence.RepSamples, evidence.BenchstatOutput = nil, ""
	m.comparisons, m.pooled = nil, pooledSamples{}
	for _, runs := range m.seeds {
		e.recordSeedMetrics(ctx, runs.seed.ID, runs.ab, evidence, m)
	}
	e.finalizeCandidateEvidence(ctx, evidence, m)
}

func confirmationNote(unconfirmed []domain.MetricComparison, limit float64) string {
	readings := make([]string, 0, len(unconfirmed))
	for _, c := range unconfirmed {
		name := "pooled"
		if c.Workload != "" {
			name = c.Workload
		}
		readings = append(readings, fmt.Sprintf("%s %+.2f%%", name, c.DeltaPercent))
	}
	return fmt.Sprintf("%s over the %.2f%% limit without significance after %d pairs, so every workload was measured over %d more", strings.Join(readings, ", "), limit, measurementRepetitions, measurementRepetitions)
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

func (e *Engine) recordSeedMetrics(ctx context.Context, seedID string, ab runner.ABResult, evidence *orchestrator.CandidateEvidence, m *measurement) {
	wallComparisons, wallBenchstat := e.compareWallTimeMetric(ctx, seedID, ab.Baseline, ab.Candidate)
	m.comparisons = append(m.comparisons, wallComparisons...)
	baseSamples := metricValues(ab.Baseline, wallTime)
	candSamples := metricValues(ab.Candidate, wallTime)
	evidence.RepSamples = append(evidence.RepSamples, domain.WorkloadSamples{Workload: seedID, BaselineNs: baseSamples, CandidateNs: candSamples})
	if wallBenchstat != "" {
		if evidence.BenchstatOutput != "" {
			evidence.BenchstatOutput += "\n\n"
		}
		evidence.BenchstatOutput += fmt.Sprintf("workload %s:\n%s", seedID, wallBenchstat)
	}
	pooled := &m.pooled
	pooled.cpuBase = append(pooled.cpuBase, metricValues(ab.Baseline, cpuTime))
	pooled.memBase = append(pooled.memBase, metricValues(ab.Baseline, peakMemory))
	pooled.cpuCand = append(pooled.cpuCand, metricValues(ab.Candidate, cpuTime))
	pooled.memCand = append(pooled.memCand, metricValues(ab.Candidate, peakMemory))
	pooled.wallBase = append(pooled.wallBase, baseSamples)
	pooled.wallCand = append(pooled.wallCand, candSamples)
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

func (e *Engine) finalizeCandidateEvidence(ctx context.Context, evidence *orchestrator.CandidateEvidence, m *measurement) {
	comparisons, pooled := append([]domain.MetricComparison{}, m.comparisons...), m.pooled
	// Policy looks up canonical metric names ("wall_time_ns" primary plus
	// required guardrails), so the representative workloads are folded into
	// one comparison per metric. Folding happens per repetition, not by
	// concatenating raw samples: see pooledSamples for why the concatenated
	// form turned a measured 20.8% win into an inconclusive verdict. Raw
	// per-workload data remains in RepSamples for diagnosis.
	wallComparisons, wallBenchstat := e.compareWallTimeMetric(ctx, "", pooledAsRuns(meanPerRepetition(pooled.wallBase), "wall_time_ns"), pooledAsRuns(meanPerRepetition(pooled.wallCand), "wall_time_ns"))
	comparisons = append(comparisons, wallComparisons...)
	if wallBenchstat != "" {
		evidence.BenchstatOutput += "pooled representative workloads:\n" + wallBenchstat
	}
	comparisons = append(comparisons, compareMetric("", "cpu_time_ns", "ns",
		pooledAsRuns(meanPerRepetition(pooled.cpuBase), "cpu_time_ns"), pooledAsRuns(meanPerRepetition(pooled.cpuCand), "cpu_time_ns"), cpuTime)...)
	comparisons = append(comparisons, compareMetric("", "peak_memory_bytes", "bytes",
		pooledAsRuns(maxPerRepetition(pooled.memBase), "peak_memory_bytes"), pooledAsRuns(maxPerRepetition(pooled.memCand), "peak_memory_bytes"), peakMemory)...)
	if m.sizeErr == nil {
		comparisons = append(comparisons, domain.MetricComparison{Metric: "binary_size_bytes", Unit: "bytes", Baseline: float64(m.baselineSize), Candidate: float64(m.candSize), DeltaPercent: percentDelta(float64(m.baselineSize), float64(m.candSize)), StatisticallyFit: true, Significant: m.baselineSize != m.candSize})
	}
	evidence.BehaviorMatches = true
	evidence.SafetyChecksPassed = true
	evidence.RepresentativeEvidence = len(m.seeds) > 0
	evidence.Comparisons = comparisons
	evidence.Summary = fmt.Sprintf("patched tree built; tests passed; %d representative workload(s) measured over %d A/B pairs each", len(m.seeds), len(m.seeds[0].ab.Baseline))
}

// seedMeasurementRequest builds the measurement runner request for one seed
// workload against a specific binary. The workload ID is deterministic per
// campaign and seed so baseline and variant runs share it.
// seedMeasurementRequest builds the measurement request for one seed. The
// workload id it embeds is the derived run identifier the artifacts are keyed
// by; comparisons and reports name the workload by the seed id instead, so
// callers no longer need it back.
func (e *Engine) seedMeasurementRequest(seed manifest.SeedWorkload, buildID, binaryPath string) runner.RunRequest {
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
		Workload:      domain.Workload{ID: wid, Name: seed.Name, Seed: seed.ID, Tier: seed.Tier, Command: domain.Command{Path: binaryPath, Args: args}, Timeout: timeout},
		Mode:          domain.RunModeMeasurement,
		Stdin:         []byte(seed.Stdin),
		Fixtures:      fixtures,
		AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"},
		// Isolated campaigns grant neither. Without local isolation
		// (TestingUnsafeDisableIsolation) there is no guard to enforce either
		// restriction, so the request grants both, as discovery's does.
		NetworkAllowed:    !e.state.LocalIsolation,
		FilesystemAllowed: !e.state.LocalIsolation,
	}
	return req
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
	if reason := e.pgoLaneUnaffordable(); reason != "" {
		e.skipPgoLane(evidence, reason)
		return
	}
	buildCtx, cancelBuild := context.WithTimeout(ctx, e.pgoBuildBudget())
	defer cancelBuild()
	started := e.now()
	baselinePgo, candidatePgo, ok := e.buildPgoBinaries(buildCtx, candidateWorktree, candidateID, evidence)
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
	evidence.PgoNote = fmt.Sprintf("informational PGO comparison over %d representative workload(s), %d A/B pairs each, lane cost %s; both sides built with -pgo=%s from the discovery CPU profile; this lane never changes accept/reject decisions", measured, measurementRepetitions, e.now().Sub(started).Round(time.Second), filepath.Base(e.state.PGOProfilePath))
	_ = e.saveEvent("pgo_lane_completed", evidence.PgoNote, nil)
}

// pgoBuildBudget is the wall-clock bound on one profile-guided build in the
// informational lane.
func (e *Engine) pgoBuildBudget() time.Duration {
	if e.pgoBuildTimeout > 0 {
		return e.pgoBuildTimeout
	}
	return pgoLaneBuildTimeout
}

// pgoLaneUnaffordable reports why the informational lane should not start, or
// an empty string when the campaign can afford it. A campaign with no duration
// bound has nothing to protect.
func (e *Engine) pgoLaneUnaffordable() string {
	budget := e.state.Manifest.Campaign.MaxDuration.Duration()
	if budget <= 0 {
		return ""
	}
	floor := 3 * e.pgoBuildBudget()
	remaining := budget - e.elapsedRunTime(e.now())
	if remaining >= floor {
		return ""
	}
	return fmt.Sprintf("campaign has %s of its %s budget left and the lane may spend up to %s", remaining.Round(time.Second), budget, floor)
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
		// A build the lane's own budget cut short is a statement about the
		// lane, not about the target's compiler output.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			e.skipPgoLane(evidence, fmt.Sprintf("%s build exceeded the lane's %s budget", label, e.pgoBuildBudget()))
			return false
		}
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
		baseReq := e.seedMeasurementRequest(seed, candidateID+"-baseline-pgo", baselinePgo)
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
		comparisons = append(comparisons, compareMetric(seed.ID, "wall_time_ns", "ns", ab.Baseline, ab.Candidate, wallTime)...)
		comparisons = append(comparisons, compareMetric(seed.ID, "cpu_time_ns", "ns", ab.Baseline, ab.Candidate, cpuTime)...)
		comparisons = append(comparisons, compareMetric(seed.ID, "peak_memory_bytes", "bytes", ab.Baseline, ab.Candidate, peakMemory)...)
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

// meanPerRepetition folds per-workload sample series into one series holding
// the mean across workloads for each repetition.
//
// Wall and CPU time are additive, so this is the average cost of one pass over
// the representative workload set, which is the quantity the primary metric
// threshold is about. Dividing every sample by a constant leaves a Welch t
// unchanged, so the reported absolute values stay on the single-run scale
// while the test no longer counts between-workload spread as noise. Series of
// differing length are folded over their common prefix.
func meanPerRepetition(series [][]float64) []float64 {
	depth := commonDepth(series)
	if depth == 0 {
		return nil
	}
	out := make([]float64, depth)
	for i := range out {
		sum := 0.0
		for _, s := range series {
			sum += s[i]
		}
		out[i] = sum / float64(len(series))
	}
	return out
}

// maxPerRepetition folds per-workload series into one series holding the
// largest value per repetition. Peak memory is a high-water mark rather than
// an additive quantity: the sum of two workloads' peaks is a number no run
// ever produced.
func maxPerRepetition(series [][]float64) []float64 {
	depth := commonDepth(series)
	if depth == 0 {
		return nil
	}
	out := make([]float64, depth)
	for i := range out {
		out[i] = series[0][i]
		for _, s := range series[1:] {
			out[i] = max(out[i], s[i])
		}
	}
	return out
}

func commonDepth(series [][]float64) int {
	if len(series) == 0 {
		return 0
	}
	depth := len(series[0])
	for _, s := range series[1:] {
		depth = min(depth, len(s))
	}
	return depth
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
func compareMetric(workload, metric, unit string, baseline, candidateRuns []domain.RunResult, selector metricSelector) []domain.MetricComparison {
	baseVals := collectMetric(baseline, selector)
	candVals := collectMetric(candidateRuns, selector)
	result := domain.MetricComparison{Metric: metric, Workload: workload, Unit: unit}
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
	result.Significant = statisticallySupported(baseVals, candVals)
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
	t := math.Abs(ma-mb) / se
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
	case domain.PolicyIdiomatic:
		// Idiomatic optimization forbids no technique: it is the default lane
		// where the patch must stand on ordinary source changes alone.
		return nil
	case domain.PolicySpecialized:
		return []string{"unsafe.", "assembly", "cgo"}
	case domain.PolicyNative:
		return []string{"cgo"}
	default:
		return nil
	}
}
