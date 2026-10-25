package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/candidate"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/policy"
	"github.com/asaf-shitrit/gotorque/internal/runner"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
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

// evalSettings is what differs between the callers of one evaluation: a
// campaign's attempt, a verification, and a null candidate.
type evalSettings struct {
	// pairs is the interleaved pair count per workload and series.
	pairs int
	// refuseRepeats turns on the pre-build refusals of a patch already measured
	// or a function already fixed. Verification re-measures a known patch on
	// purpose.
	refuseRepeats bool
	// pgoLane runs the informational profile-guided lane after an ordinary
	// verdict.
	pgoLane bool
	// baseline is the set of tests the test gate holds a candidate to. Nil
	// means the campaign's own, which the gate may narrow and persist; a
	// verification passes a copy it can narrow without touching the
	// campaign's.
	baseline testBaseline
	// known is what the campaign has already measured and accepted, for the
	// refusals refuseRepeats turns on.
	known knownCandidates
}

// evaluate runs the deterministic half of the candidate loop: validate the
// proposed diff, apply it in an isolated worktree, build, gate on the upstream
// test suite, then measure baseline and candidate binaries interleaved on
// representative seed workloads. Every terminal judgment stays here or in
// policy; the model never self-approves.
func (ev *evaluator) evaluate(ctx context.Context, req orchestrator.CandidateRequest, s evalSettings) (orchestrator.CandidateEvidence, error) {
	if err := ev.requireFreeSpace(); err != nil {
		return orchestrator.CandidateEvidence{}, err
	}
	defer ev.releaseBase(ctx)
	patchText, transport, err := ev.resolveCandidatePatch(ctx, req)
	if err != nil {
		return ev.rejectUnresolvedPatch(req, transport, err), nil
	}
	id, patchPath, err := ev.writeCandidatePatch(req, patchText)
	if err != nil {
		return orchestrator.CandidateEvidence{}, err
	}
	evidence := orchestrator.CandidateEvidence{
		Candidate:    domain.Candidate{ID: id, BaseRevision: ev.baseRevision, Hypothesis: req.Proposal.Hypothesis, PatchPath: patchPath, Transport: transport},
		ArtifactURIs: []string{patchPath},
	}
	prepared, ok := ev.prepareCandidate(ctx, req, patchPath, &evidence)
	if !ok {
		return evidence, nil
	}
	// prepareCandidate replaces evidence.Candidate wholesale with the one
	// WorktreeManager.Prepare built, which does not know the transport.
	evidence.Candidate.Transport = transport
	// Worktree teardown runs even when the caller's context is already
	// canceled (duration budget or Ctrl-C), but keeps its values.
	defer func() { _ = prepared.Close(context.WithoutCancel(ctx)) }()
	ev.judgePrepared(ctx, req, s, prepared.Worktree, &evidence)
	return evidence, nil
}

// rejectUnresolvedPatch is the evidence for a candidate whose diff could not be
// built from function_source: a pre-build rejection like a patch that does not
// parse, marked unmeasured so the target is offered once more (ADR 0017). It
// still needs an ID: the orchestrator stops the whole campaign on evidence
// without one.
func (ev *evaluator) rejectUnresolvedPatch(req orchestrator.CandidateRequest, transport string, err error) orchestrator.CandidateEvidence {
	id := stableID("candidate", ev.campaignID, strconv.Itoa(req.Attempt), transport, err.Error())
	// Kept like any other answer: held-out dyff lost two attempts here to
	// "function_source must be a function declaration", and with nothing
	// written there was no way to see what the optimizer had sent.
	ev.keepProposal(id, req.Proposal)
	return orchestrator.CandidateEvidence{
		Candidate:     domain.Candidate{ID: id, BaseRevision: ev.baseRevision, Hypothesis: req.Proposal.Hypothesis, Transport: transport},
		Summary:       fmt.Sprintf("candidate rejected before build: %v", err),
		FailureDetail: tail(err.Error(), 400),
		Unmeasured:    true,
	}
}

// judgePrepared runs everything after the patch applied: the repeat and shape
// refusals, the build, the test gate, the measurement and the PGO lane. It
// stops at the first stage that ends the candidate and leaves why in evidence.
func (ev *evaluator) judgePrepared(ctx context.Context, req orchestrator.CandidateRequest, s evalSettings, worktree string, evidence *orchestrator.CandidateEvidence) {
	id := evidence.Candidate.ID
	if s.refuseRepeats && rejectMeasuredDuplicate(s.known, id, evidence) {
		return
	}
	if !ev.patchHasShape(ctx, s, worktree, req.Target, evidence) {
		return
	}
	candidateBinary, ok := ev.buildAndTestCandidate(ctx, worktree, id, evidence)
	if !ok {
		return
	}
	if !ev.measureAndFinalize(ctx, s, evidence, id, candidateBinary) {
		return
	}
	// Informational PGO lane: only candidates that passed the test suite and
	// produced a full ordinary verdict reach it, so the extra builds and A/B
	// series are never spent on rejected work. A verification turns it off: its
	// record has no PGO columns to fill.
	if s.pgoLane {
		ev.runPgoLane(ctx, s, evidence, worktree, id)
	}
}

func (ev *evaluator) writeCandidatePatch(req orchestrator.CandidateRequest, patchText string) (id, patchPath string, err error) {
	patchDir := filepath.Join(ev.dir, "patches")
	if err := os.MkdirAll(patchDir, 0o700); err != nil {
		return "", "", err
	}
	id = stableID("candidate", ev.campaignID, strconv.Itoa(req.Attempt), patchText)
	patchPath = filepath.Join(patchDir, id+".diff")
	if err := os.WriteFile(patchPath, []byte(patchText), 0o600); err != nil {
		return "", "", err
	}
	ev.keepProposal(id, req.Proposal)
	return id, patchPath, nil
}

// keepProposal writes the optimizer's answer as it arrived to
// patches/<id>.proposal.json, best effort. Two cue candidates were rejected as
// malformed diffs, and without the answer nothing said whether a
// function_source had come with them.
func (ev *evaluator) keepProposal(id string, proposal agents.OptimizerResult) {
	dir := filepath.Join(ev.dir, "patches")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if data, err := json.MarshalIndent(proposal, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, id+".proposal.json"), data, 0o600)
	}
}

func (ev *evaluator) prepareCandidate(ctx context.Context, req orchestrator.CandidateRequest, patchPath string, evidence *orchestrator.CandidateEvidence) (*candidate.Prepared, bool) {
	prohibited := prohibitedTechniquesFor(ev.manifest.OptimizationPolicy)
	manager := &candidate.WorktreeManager{Toolchain: ev.toolchain, Repository: ev.repository, Root: filepath.Join(ev.dir, "worktrees")}
	prepared, err := manager.Prepare(ctx, ev.baseRevision, patchPath, req.Proposal.Hypothesis, candidate.Policy{ProhibitedTechniques: prohibited})
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
// rejectMeasuredDuplicate rejects, before it is built, a candidate whose
// patch this revision has already measured, in this campaign or in one
// --history named (ADR 0031). The worktree manager's candidate ID is a digest
// of the base revision and the normalized patch, so an equal ID is the same
// patch. On dasel the WithExecutorID concatenation rewrite came back
// byte-identical under a second cause's target and spent a whole measurement
// to repeat an inconclusive verdict. It is marked unmeasured, so the target is
// offered once more with the reason in prior_candidates.
func rejectMeasuredDuplicate(known knownCandidates, id string, evidence *orchestrator.CandidateEvidence) bool {
	where, ok := known.measuredAt(id)
	if !ok {
		return false
	}
	evidence.Summary = "candidate rejected before build: the identical patch was already measured on this revision (" + where + "); propose a different change"
	evidence.FailureDetail = where
	evidence.Unmeasured = true
	return true
}

// rejectKnownAcceptedFix rejects, before it is built, a patch that edits a
// function an accepted candidate already sped up, in this campaign or in a
// --history campaign (ADR 0031). An accepted fix is not applied to the base
// revision, so the next campaign can find it again: overnight-gojq-history-1
// spent its last attempt re-finding overnight-gojq-1's printValues
// buffering fix as a different patch, and overnight-go-jsonnet-history-1
// re-found checkArguments, although both reached the optimizer as accepted
// earlier_candidates. Both were free-choice candidates, and only those are
// held to this: a code-chosen target on an already-fixed function carries a
// different cause (overnight-gojq-1 attempt 3 attacked printValues' redundant
// work after its buffering fix was accepted), and since accepted fixes are
// not stacked, measuring it alone is still a fair question. It is marked
// unmeasured, like every pre-build rejection, and names where the fix was
// accepted.
func rejectKnownAcceptedFix(known knownCandidates, worktree string, diff []byte, evidence *orchestrator.CandidateEvidence) bool {
	changes := parseChanges(diff)
	for _, fix := range known.fixes {
		if callerTouched(worktree, changes, agents.FunctionRef{Name: fix.Function, Location: fix.Location}) {
			evidence.Summary = fmt.Sprintf("candidate rejected before build: %s already has an accepted fix (%s); propose a change to a different function", fix.Function, fix.Where)
			evidence.FailureDetail = fix.Where
			evidence.Unmeasured = true
			return true
		}
	}
	return false
}

// patchHasShape runs checkShape on the applied worktree. A diff Git cannot
// produce leaves the judgment to the build, which would fail on the same
// tree; the check only ever adds a reason, never a pass.
func (ev *evaluator) patchHasShape(ctx context.Context, s evalSettings, worktree string, target *agents.Target, evidence *orchestrator.CandidateEvidence) bool {
	diff, err := ev.toolchain.ChangedLines(ctx, worktree)
	if err != nil {
		return true
	}
	if target == nil && s.refuseRepeats && rejectKnownAcceptedFix(s.known, worktree, diff, evidence) {
		return false
	}
	if err := checkShape(worktree, diff, target); err != nil {
		evidence.Summary = fmt.Sprintf("candidate rejected before build: patch shape: %v", err)
		evidence.FailureDetail = tail(err.Error(), 400)
		evidence.Unmeasured = true
		return false
	}
	return true
}

func (ev *evaluator) buildAndTestCandidate(ctx context.Context, worktree, id string, evidence *orchestrator.CandidateEvidence) (string, bool) {
	binDir := filepath.Join(ev.dir, "builds")
	candidateBinary := filepath.Join(binDir, id+"-"+filepath.Base(ev.manifest.Target.Build.Binary))
	if !ev.buildCandidateBinary(ctx, worktree, candidateBinary, evidence) {
		return "", false
	}
	evidence.ArtifactURIs = append(evidence.ArtifactURIs, candidateBinary)
	if !ev.candidateTestsPassed(ctx, worktree, evidence) {
		return "", false
	}
	return candidateBinary, true
}

func (ev *evaluator) buildCandidateBinary(ctx context.Context, worktree, candidateBinary string, evidence *orchestrator.CandidateEvidence) bool {
	buildResult, buildErr := ev.toolchain.Build(ctx, toolchain.BuildRequest{Repository: worktree, Directory: ev.manifest.Target.Build.Directory, Target: ev.manifest.Target.Build.Package, Output: candidateBinary, Env: []string{"GOTOOLCHAIN=local"}})
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

func (ev *evaluator) candidateTestsPassed(ctx context.Context, worktree string, evidence *orchestrator.CandidateEvidence) bool {
	// Behavior gate: the upstream test suite must not regress against the
	// unpatched revision. Failures that predate the patch are subtracted
	// rather than charged to it. A clean exit is classified too: it is what a
	// suite reports after a test the baseline passed was skipped or dropped.
	testResult, testErr := ev.toolchain.Test(ctx, toolchain.TestRequest{Repository: worktree, JSON: true, Env: []string{"GOTOOLCHAIN=local"}})
	baseline := ev.baseline
	reason, passed := classifyTestOutcome(baseline, testResult, testErr)
	if !passed && ev.pruneUnstablePasses(ctx, baseline, testResult) {
		reason, passed = classifyTestOutcome(baseline, testResult, testErr)
	}
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

func (ev *evaluator) measureAndFinalize(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string) bool {
	m := measurement{comparisons: make([]domain.MetricComparison, 0, 4)}
	host := ev.machine
	if ev.localIsolation {
		evidence.QuietWait, evidence.QuietWaitExpired = host.waitQuiet(ctx)
	}
	evidence.LoadAverages = host.load()
	defer func() {
		evidence.LoadAverages = append(evidence.LoadAverages, host.load()...)
		evidence.LoadContended = host.contended(evidence.LoadAverages)
	}()
	m.baselineSize, m.candSize, m.sizeErr = binarySizes(ev.binaryPath, candidateBinary)
	if !ev.measureSeedsOnQuietMachine(ctx, s, evidence, id, candidateBinary, &m) {
		return false
	}
	if len(m.seeds) == 0 {
		evidence.Summary = "no representative seed workloads available for measurement"
		evidence.Comparisons = m.comparisons
		return false
	}
	ev.finalizeCandidateEvidence(ctx, evidence, &m)
	evidence.ValidationJobs = append(evidence.ValidationJobs, "build", "test-suite", "interleaved-ab")
	return ev.secondLook(ctx, s, evidence, id, candidateBinary, &m)
}

// secondLook runs the steps a candidate gets after its first series, in the
// order that keeps a per-workload accept off a pooled reading. The two steps
// that extend every seed run first, and the step that replaces one workload's
// series runs last: an extending step adds its own pairs to the workload that
// selected the accept, so a per-workload accept must be re-measured after it,
// not before (ADR 0041, ADR 0021, ADR 0034).
func (ev *evaluator) secondLook(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	if !ev.confirmRegressions(ctx, s, evidence, id, candidateBinary, m) {
		return false
	}
	if !ev.confirmImprovements(ctx, s, evidence, id, candidateBinary, m) {
		return false
	}
	return ev.confirmSelection(ctx, s, evidence, id, candidateBinary, m)
}

// measureSeedsOnQuietMachine measures every representative seed, and
// measures them all again from scratch, once, when the machine was contended
// by the time the first pass ended. The wait before measuring only protects
// the start: on overnight-miller-4 the load went from 6.4 to 16.6 during a
// candidate's pairs, and its workload readings swung from -9% to +12%. The
// same kind of burst gave overnight-miller-1 a false acceptance. The first
// pass's samples are discarded, the engine waits for quiet again, and only the
// second pass reaches the verdict; its load is what the record reports, with
// the discarded pass's load kept beside it. A second contended pass is kept
// as it is, flagged.
func (ev *evaluator) measureSeedsOnQuietMachine(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	before, fresh := *evidence, *m
	if !ev.measureSeedWorkloads(ctx, s, evidence, id, candidateBinary, m) {
		return false
	}
	host := ev.machine
	mid := host.load()
	if !ev.localIsolation || !host.contended(mid) {
		return true
	}
	waited, expired := host.waitQuiet(ctx)
	*evidence, *m = before, fresh
	evidence.QuietWait += waited
	evidence.QuietWaitExpired = expired
	evidence.DiscardedLoad = slices.Concat(before.LoadAverages, mid)
	evidence.LoadAverages = host.load()
	return ev.measureSeedWorkloads(ctx, s, evidence, id, candidateBinary, m)
}

func (ev *evaluator) measureSeedWorkloads(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	for _, seed := range ev.manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		if !ev.measureOneSeed(ctx, s, seed, id, candidateBinary, evidence, m) {
			return false
		}
	}
	return true
}

func (ev *evaluator) measureOneSeed(ctx context.Context, s evalSettings, seed manifest.SeedWorkload, id, candidateBinary string, evidence *orchestrator.CandidateEvidence, m *measurement) bool {
	baseReq, candReq := ev.abRequests(seed, id, candidateBinary)
	// First-execution warm-up: a freshly built binary pays a one-time OS
	// cost on its first exec (Gatekeeper scan, page-in) that otherwise
	// poisons repetition 0 of the candidate leg — observed as ~470ms vs
	// ~9ms steady state. Discarded, errors ignored.
	_, _ = ev.runner.Run(ctx, candReq)
	runs := seedRuns{seed: seed, deterministic: outputIsDeterministic(ctx, ev.runner, baseReq)}
	if !ev.runSeries(ctx, s, &runs, id, candidateBinary, evidence, m.comparisons) {
		return false
	}
	m.seeds = append(m.seeds, runs)
	ev.recordSeedMetrics(ctx, seed.ID, runs.ab, evidence, m)
	return true
}

// abIsolationNotes collects isolation notes from every baseline and
// candidate run in one interleaved series. Baseline and candidate share one
// SandboxPolicy (see runner.SandboxPolicy), so any degradation applies to
// both legs identically; this just surfaces it once for the report.
func abIsolationNotes(ab runner.ABResult) []string {
	var notes []string
	for _, r := range ab.Baseline {
		notes = append(notes, r.IsolationNotes...)
	}
	for _, r := range ab.Candidate {
		notes = append(notes, r.IsolationNotes...)
	}
	return notes
}

func (ev *evaluator) abRequests(seed manifest.SeedWorkload, id, candidateBinary string) (runner.RunRequest, runner.RunRequest) {
	baseReq := seedMeasurementRequest(ev.manifest, ev.campaignID, seed, ev.buildID, ev.binaryPath)
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
func (ev *evaluator) runSeries(ctx context.Context, s evalSettings, runs *seedRuns, id, candidateBinary string, evidence *orchestrator.CandidateEvidence, comparisons []domain.MetricComparison) bool {
	baseReq, candReq := ev.abRequests(runs.seed, id, candidateBinary)
	ab, err := ev.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: s.pairs})
	ev.journal.isolationNotes(abIsolationNotes(ab))
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
func (ev *evaluator) confirmRegressions(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	config := policyConfigFromManifest(ev.manifest)
	unconfirmed := policy.UnconfirmedRegressions(config, eligibleReadings(config, evidence.Comparisons))
	// A guardrail past its limit without significance gets the same second
	// series: the policy no longer rejects on its point estimate, so more
	// samples are what lets a real regression become significant.
	unconfirmed = append(unconfirmed, policy.UnconfirmedGuardrails(config, evidence.Comparisons)...)
	// A peak-memory breach that is significant still gets a series of its own:
	// the policy rejects on it only when a fresh series reproduces it, because
	// one contended run or one GC step moves a high-water mark by megabytes
	// without the candidate costing more memory (see policy.replicatedGuardrail).
	replicate := policy.GuardrailsToReplicate(config, evidence.Comparisons)
	// Every over-limit reading of a re-measured guardrail gets the fresh
	// series' own reading recorded, not only the supported breaches that ask
	// for the series themselves: an over-limit reading without significance is
	// re-measured as an unconfirmed guardrail, and folding the fresh series in
	// can be what makes it significant. The policy then needs that reading to
	// decide whether the breach reproduces (ADR 0040); leaving it out lets the
	// verdict reject on the pooled series alone.
	overLimit := policy.ReplicatedGuardrailsOverLimit(config, evidence.Comparisons)
	if len(unconfirmed) == 0 && len(replicate) == 0 {
		return true
	}
	// The readings the first series produced for the guardrails a fresh series
	// re-measures, kept before both series are pooled into Comparisons: the
	// verdict's reasons say what the first series read, and the pooled value is
	// no longer that number.
	firstSeries := policy.ReplicatedGuardrails(config, evidence.Comparisons)
	starts := seriesStarts(m.seeds)
	for i := range m.seeds {
		if !ev.runSeries(ctx, s, &m.seeds[i], id, candidateBinary, evidence, evidence.Comparisons) {
			return false
		}
	}
	ev.rederive(ctx, evidence, m)
	evidence.FirstSeriesGuardrails = firstSeries
	notes := make([]string, 0, 2)
	if len(unconfirmed) > 0 {
		notes = append(notes, confirmationNote(unconfirmed, config.MaximumGuardrailRegressionPercent, config.PrimaryMetric, s.pairs))
	}
	if len(overLimit) > 0 {
		evidence.FreshGuardrails = ev.freshGuardrailComparisons(m.seeds, starts)
	}
	if len(replicate) > 0 {
		notes = append(notes, replicationNote(replicate, config.MaximumGuardrailRegressionPercent, s.pairs))
	}
	evidence.Summary += "; " + strings.Join(notes, "; ")
	evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	_ = ev.journal.event("measurement_confirmed", strings.Join(notes, "; "), nil)
	return true
}

// seriesStarts records, per seed, how many runs each side already held when a
// confirmation series was about to start, so the new series can be judged on
// its own.
func seriesStarts(seeds []seedRuns) []int {
	starts := make([]int, len(seeds))
	for i, runs := range seeds {
		starts[i] = len(runs.ab.Baseline)
	}
	return starts
}

// freshGuardrailComparisons derives the pooled CPU-time and peak-memory
// readings of one fresh series: every seed's runs after the offset it held
// before the series started. Folding, not concatenation, is how the pooled
// reading is derived everywhere else (pooledSamples).
func (ev *evaluator) freshGuardrailComparisons(seeds []seedRuns, starts []int) []domain.MetricComparison {
	var pooled pooledSamples
	for i, runs := range seeds {
		start := 0
		if i < len(starts) {
			start = min(starts[i], len(runs.ab.Baseline))
		}
		base, cand := runs.ab.Baseline[start:], runs.ab.Candidate[start:]
		pooled.cpuBase = append(pooled.cpuBase, metricValues(base, cpuTime))
		pooled.cpuCand = append(pooled.cpuCand, metricValues(cand, cpuTime))
		pooled.memBase = append(pooled.memBase, metricValues(base, peakMemory))
		pooled.memCand = append(pooled.memCand, metricValues(cand, peakMemory))
	}
	return []domain.MetricComparison{
		compareMetric("", "cpu_time_ns", "ns", ev.regressionLimit("cpu_time_ns"),
			pooledAsRuns(meanPerRepetition(pooled.cpuBase), "cpu_time_ns"), pooledAsRuns(meanPerRepetition(pooled.cpuCand), "cpu_time_ns"), cpuTime)[0],
		compareMetric("", "peak_memory_bytes", "bytes", ev.regressionLimit("peak_memory_bytes"),
			pooledAsRuns(maxPerRepetition(pooled.memBase), "peak_memory_bytes"), pooledAsRuns(maxPerRepetition(pooled.memCand), "peak_memory_bytes"), peakMemory)[0],
	}
}

// replicationNote says which guardrail breaches a fresh series was measured
// to check.
func replicationNote(breaches []domain.MetricComparison, limit float64, pairs int) string {
	readings := make([]string, 0, len(breaches))
	for _, c := range breaches {
		readings = append(readings, fmt.Sprintf("%s %+.2f%%", c.Metric, c.DeltaPercent))
	}
	return fmt.Sprintf("%s over the %.2f%% limit and significant after %d pairs, so a fresh series of %d pairs was measured to check it reproduces", strings.Join(readings, ", "), limit, pairs, pairs)
}

// confirmImprovements measures every representative seed again when the
// candidate would otherwise end inconclusive only because an
// acceptance-eligible reading improved by at least the manifest's minimum but
// lacks statistical support, then derives every comparison from both series
// and lets the unchanged policy decide (ADR 0021). This mirrors
// confirmRegressions on the acceptance side: on live yq campaigns
// (~20ms workloads, 25 pairs) candidates read per-workload improvements of
// 11.55%, 9.51% and 5.39% that were not statistically supported and were
// discarded as inconclusive, because 25 pairs cannot separate a 5-10% effect
// from noise at that duration.
//
// At most one extra series runs per candidate in total: if the regression
// confirmation already extended every seed, this reuses that series rather
// than running a third one.
func (ev *evaluator) confirmImprovements(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	if slices.Contains(evidence.ValidationJobs, "interleaved-ab-confirmation") {
		return true
	}
	config := policyConfigFromManifest(ev.manifest)
	unconfirmed, note := improvementsToConfirm(config, s.pairs, evidence)
	if len(unconfirmed) == 0 {
		return true
	}
	for i := range m.seeds {
		if !ev.runSeries(ctx, s, &m.seeds[i], id, candidateBinary, evidence, evidence.Comparisons) {
			return false
		}
	}
	ev.rederive(ctx, evidence, m)
	evidence.Summary += "; " + note
	evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	_ = ev.journal.event("improvement_confirmed", note, nil)
	return true
}

// rederive computes every comparison again from the runs measured so far.
func (ev *evaluator) rederive(ctx context.Context, evidence *orchestrator.CandidateEvidence, m *measurement) {
	evidence.RepSamples, evidence.BenchstatOutput = nil, ""
	m.comparisons, m.pooled = nil, pooledSamples{}
	for _, runs := range m.seeds {
		ev.recordSeedMetrics(ctx, runs.seed.ID, runs.ab, evidence, m)
	}
	ev.finalizeCandidateEvidence(ctx, evidence, m)
}

func confirmationNote(unconfirmed []domain.MetricComparison, limit float64, primary string, pairs int) string {
	readings := make([]string, 0, len(unconfirmed))
	for _, c := range unconfirmed {
		name := "pooled"
		if c.Workload != "" {
			name = c.Workload
		}
		if c.Metric != "" && c.Metric != primary {
			name = c.Metric
		}
		readings = append(readings, fmt.Sprintf("%s %+.2f%%", name, c.DeltaPercent))
	}
	return fmt.Sprintf("%s over the %.2f%% limit without significance after %d pairs, so every workload was measured over %d more", strings.Join(readings, ", "), limit, pairs, pairs)
}

// improvementsToConfirm returns the readings a second series should settle,
// with the note that explains it: an unsupported improvement past the
// minimum on a candidate that would end inconclusive (ADR 0021), or the
// borderline improvement an accept would rest on (ADR 0034).
func improvementsToConfirm(config policy.Config, pairs int, evidence *orchestrator.CandidateEvidence) ([]domain.MetricComparison, string) {
	eligible := eligibleReadings(config, evidence.Comparisons)
	switch evaluateEvidence(config, evidence).Decision {
	case domain.DecisionInconclusive:
		unconfirmed := policy.UnconfirmedImprovements(config, eligible)
		return unconfirmed, improvementConfirmationNote(unconfirmed, config.MinimumImprovementPercent, pairs)
	case domain.DecisionAccepted:
		borderline := policy.BorderlineImprovements(config, eligible)
		return borderline, borderlineConfirmationNote(borderline, config.MinimumImprovementPercent, pairs)
	case domain.DecisionRejected:
		return nil, ""
	}
	return nil, ""
}

// evaluateEvidence judges a candidate's evidence exactly as the verdict will,
// so a confirmation step can ask what the policy would decide before it spends
// another series.
func evaluateEvidence(config policy.Config, evidence *orchestrator.CandidateEvidence) policy.Result {
	return policy.Evaluate(config, policy.Evidence{
		BehaviorMatches:        evidence.BehaviorMatches,
		FailureSummary:         evidence.Summary,
		SafetyChecksPassed:     evidence.SafetyChecksPassed,
		RepresentativeEvidence: evidence.RepresentativeEvidence,
		Comparisons:            evidence.Comparisons,
		Primary:                eligibleReadings(config, evidence.Comparisons),
		FreshGuardrails:        evidence.FreshGuardrails,
		FirstSeriesGuardrails:  evidence.FirstSeriesGuardrails,
	})
}

// confirmSelection measures the workload an accept would rest on again from
// scratch when that reading is one workload's rather than the pooled metric,
// and lets the verdict rest on the fresh series alone (ADR 0041). A per-workload
// accept is the best of several readings, so the reading that cleared the bar
// may have cleared it on chance: on daily-2026-10-02-devB-miller attempt 4 the
// campaign accepted a -6.51% reading on one of five workloads that `gotorque
// verify` put at -3.04% over sixty fresh pairs. The series that selected the
// workload is discarded, because pooling the two keeps part of that bias; every
// comparison is derived again from the fresh runs, so the record holds the
// series the verdict rests on.
//
// A candidate whose regression or unsupported-improvement confirmation already
// ran is measured again here too: that confirmation extended every seed, so a
// per-workload accept would rest on both series pooled, and the pairs that
// selected the workload are still in them. The fresh series replaces the pooled
// one for the workload that carries the accept, so it is as long as what it
// replaces and the pooled reading keeps one length per seed.
//
// Each workload that would carry the accept is measured at most once more, so
// a candidate whose fresh reading no longer clears the bar but whose second
// workload's does is measured again there rather than accepted on the reading
// that selected it. The campaign deadline bounds the cost, as it bounds every
// series.
func (ev *evaluator) confirmSelection(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement) bool {
	// The bias being corrected is choosing the best of several readings: a
	// target with one representative workload offered no choice to inflate.
	if len(m.seeds) < 2 {
		return true
	}
	config := policyConfigFromManifest(ev.manifest)
	remeasured := make(map[string]bool, len(m.seeds))
	var notes []string
	for {
		selected := selectedWorkloadToRemeasure(config, evidence, remeasured)
		if len(selected) == 0 {
			break
		}
		seed := slices.IndexFunc(m.seeds, func(runs seedRuns) bool { return runs.seed.ID == selected[0].Workload })
		if seed < 0 {
			break
		}
		workload := selected[0].Workload
		note, ok := ev.remeasureSelectedWorkload(ctx, s, evidence, id, candidateBinary, m, seed, selected[0])
		if !ok {
			return false
		}
		remeasured[workload] = true
		notes = append(notes, note)
	}
	if len(notes) == 0 {
		return true
	}
	note := strings.Join(notes, "; ")
	evidence.Summary += "; " + note
	if !slices.Contains(evidence.ValidationJobs, "interleaved-ab-confirmation") {
		evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	}
	_ = ev.journal.event("selection_confirmed", note, nil)
	return true
}

// selectedWorkloadToRemeasure returns the per-workload reading an accept would
// rest on when that workload has not been measured again yet, and nothing when
// the verdict does not accept, rests on the pooled metric, or rests on a
// workload that already got its fresh series.
func selectedWorkloadToRemeasure(config policy.Config, evidence *orchestrator.CandidateEvidence, remeasured map[string]bool) []domain.MetricComparison {
	if evaluateEvidence(config, evidence).Decision != domain.DecisionAccepted {
		return nil
	}
	selected := policy.SelectedImprovements(config, eligibleReadings(config, evidence.Comparisons))
	if len(selected) == 0 || remeasured[selected[0].Workload] {
		return nil
	}
	return selected
}

// remeasureSelectedWorkload replaces one workload's runs with a fresh series of
// the length the replaced one held, derives every comparison again from what is
// now measured, and returns the note that records what the reading the verdict
// rested on said and what the fresh series alone says.
func (ev *evaluator) remeasureSelectedWorkload(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, id, candidateBinary string, m *measurement, seed int, selected domain.MetricComparison) (string, bool) {
	fresh := seedRuns{seed: m.seeds[seed].seed, deterministic: m.seeds[seed].deterministic}
	// A regression confirmation has already extended every seed to twice the
	// pair count. Re-measuring over one series of the campaign's count would
	// leave this seed shorter than the others, and the pooled reading folds the
	// seeds per repetition, so it would drop the other seeds' second series.
	replaced := len(m.seeds[seed].ab.Baseline)
	step := max(1, s.pairs)
	for remaining := replaced; remaining > 0; remaining -= step {
		series := s
		series.pairs = min(step, remaining)
		if !ev.runSeries(ctx, series, &fresh, id, candidateBinary, evidence, evidence.Comparisons) {
			return "", false
		}
	}
	m.seeds[seed] = fresh
	ev.rederive(ctx, evidence, m)
	judged := workloadReading(m.comparisons, m.seeds[seed].seed.ID, ev.manifest.Performance.PrimaryMetric)
	return selectionNote(selected, judged, replaced), true
}

// workloadReading finds one per-workload reading by the seed id the comparison
// is keyed by, or an unreadable reading when the workload has none.
func workloadReading(comparisons []domain.MetricComparison, workload, metric string) domain.MetricComparison {
	for _, comparison := range comparisons {
		if comparison.Workload == workload && comparison.Metric == metric {
			return comparison
		}
	}
	return domain.MetricComparison{Workload: workload, Metric: metric, DeltaPercent: math.NaN()}
}

// selectionNote states the selection bias and both readings: what the best of
// the workload readings claimed, and what the fresh series alone measured.
func selectionNote(selecting, judged domain.MetricComparison, pairs int) string {
	return fmt.Sprintf("workload %q read %+.2f%% as the best of the workload readings, so it alone was measured over %d fresh pairs and judged on those alone: %+.2f%%", selecting.Workload, selecting.DeltaPercent, pairs, judged.DeltaPercent)
}

func borderlineConfirmationNote(borderline []domain.MetricComparison, minimum float64, pairs int) string {
	if len(borderline) == 0 {
		return ""
	}
	c := borderline[0]
	name := "pooled"
	if c.Workload != "" {
		name = c.Workload
	}
	return fmt.Sprintf("%s improved by %.2f%%, less than twice the %.2f%% minimum, so every workload was measured over %d more before the verdict", name, -c.DeltaPercent, minimum, pairs)
}

func improvementConfirmationNote(unconfirmed []domain.MetricComparison, minimum float64, pairs int) string {
	readings := make([]string, 0, len(unconfirmed))
	for _, c := range unconfirmed {
		name := "pooled"
		if c.Workload != "" {
			name = c.Workload
		}
		readings = append(readings, fmt.Sprintf("%s %+.2f%%", name, c.DeltaPercent))
	}
	return fmt.Sprintf("%s improved past the %.2f%% minimum without significance after %d pairs, so every workload was measured over %d more", strings.Join(readings, ", "), minimum, pairs, pairs)
}

func outputIsDeterministic(ctx context.Context, r *runner.Runner, baseReq runner.RunRequest) bool {
	// Self-consistency probe: CLIs with nondeterministic tie ordering
	// (map iteration plus unstable sort) produce byte-different output
	// for identical inputs. Only when the baseline itself proves
	// deterministic do we hold the candidate to byte-exact equality;
	// otherwise the order-insensitive digest decides, so cosmetic row
	// order cannot reject a behavior-preserving patch.
	if probeA, err := r.Run(ctx, baseReq); err == nil {
		if probeB, err := r.Run(ctx, baseReq); err == nil && probeA.StdoutDigest != probeB.StdoutDigest {
			return false
		}
	}
	return true
}

func recordBehaviorMatch(ab runner.ABResult, deterministicOutput bool, seedID string, evidence *orchestrator.CandidateEvidence, comparisons []domain.MetricComparison) bool {
	// The two-run probe misses output that varies only some of the time:
	// scc's --by-file row order for files with equal counts differed in 31 of
	// 40 runs of one binary, the probe's two runs agreed often enough to call
	// it deterministic, and two live candidates were rejected for a byte
	// mismatch at repetition 11. The A/B series already holds every baseline
	// repetition, so any variation among them settles it.
	if deterministicOutput && !sameStdout(ab.Baseline) {
		deterministicOutput = false
	}
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

// sameStdout reports whether every run printed byte-identical stdout.
func sameStdout(runs []domain.RunResult) bool {
	for _, run := range runs[min(1, len(runs)):] {
		if run.StdoutDigest != runs[0].StdoutDigest {
			return false
		}
	}
	return true
}

func (ev *evaluator) recordSeedMetrics(ctx context.Context, seedID string, ab runner.ABResult, evidence *orchestrator.CandidateEvidence, m *measurement) {
	wallComparisons, wallBenchstat := ev.compareWallTimeMetric(ctx, seedID, ab.Baseline, ab.Candidate)
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

func (ev *evaluator) finalizeCandidateEvidence(ctx context.Context, evidence *orchestrator.CandidateEvidence, m *measurement) {
	comparisons, pooled := append([]domain.MetricComparison{}, m.comparisons...), m.pooled
	// Policy looks up canonical metric names ("wall_time_ns" primary plus
	// required guardrails), so the representative workloads are folded into
	// one comparison per metric. Folding happens per repetition, not by
	// concatenating raw samples: see pooledSamples for why the concatenated
	// form turned a measured 20.8% win into an inconclusive verdict. Raw
	// per-workload data remains in RepSamples for diagnosis.
	wallComparisons, wallBenchstat := ev.compareWallTimeMetric(ctx, "", pooledAsRuns(meanPerRepetition(pooled.wallBase), "wall_time_ns"), pooledAsRuns(meanPerRepetition(pooled.wallCand), "wall_time_ns"))
	comparisons = append(comparisons, wallComparisons...)
	if wallBenchstat != "" {
		evidence.BenchstatOutput += "pooled representative workloads:\n" + wallBenchstat
	}
	comparisons = append(comparisons, compareMetric("", "cpu_time_ns", "ns", ev.regressionLimit("cpu_time_ns"),
		pooledAsRuns(meanPerRepetition(pooled.cpuBase), "cpu_time_ns"), pooledAsRuns(meanPerRepetition(pooled.cpuCand), "cpu_time_ns"), cpuTime)...)
	comparisons = append(comparisons, compareMetric("", "peak_memory_bytes", "bytes", ev.regressionLimit("peak_memory_bytes"),
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
func seedMeasurementRequest(m manifest.Manifest, campaignID string, seed manifest.SeedWorkload, buildID, binaryPath string) runner.RunRequest {
	fixtures := seed.Fixtures()
	args := append(append([]string{}, m.Target.Command...), seed.Args...)
	timeout := seed.Timeout.Duration()
	if timeout == 0 {
		timeout = m.Campaign.MinimumCommandTimeout.Duration()
	}
	wid := stableID("workload", campaignID, seed.ID)
	req := runner.RunRequest{
		Build:         runner.Build{ID: buildID, BinaryPath: binaryPath},
		Workload:      domain.Workload{ID: wid, Name: seed.Name, Seed: seed.ID, Tier: seed.Tier, Command: domain.Command{Path: binaryPath, Args: args}, Timeout: timeout, ExpectedExitCode: seed.ExitCode},
		Mode:          domain.RunModeMeasurement,
		Stdin:         seed.StdinBytes(),
		Fixtures:      fixtures,
		AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"},
		// Network and filesystem access are governed by the manifest's
		// sandbox policy at the Runner level (runner.SandboxPolicy), so
		// every seed measurement -- baseline, candidate, confirmation, and
		// the PGO lane, all of which build this request through
		// seedMeasurementRequest -- shares identical isolation.
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
func (ev *evaluator) runPgoLane(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, candidateWorktree, candidateID string) {
	if ok, reason := ev.pgoProfileReady(); !ok {
		ev.skipPgoLane(evidence, reason)
		return
	}
	if reason := ev.pgoLaneUnaffordable(); reason != "" {
		ev.skipPgoLane(evidence, reason)
		return
	}
	buildCtx, cancelBuild := context.WithTimeout(ctx, ev.pgoBuildBudget())
	defer cancelBuild()
	started := ev.now()
	baselinePgo, candidatePgo, ok := ev.buildPgoBinaries(buildCtx, candidateWorktree, candidateID, evidence)
	if !ok {
		return
	}
	comparisons, measured, ok := ev.measurePgoWorkloads(ctx, s, evidence, candidateID, baselinePgo, candidatePgo)
	if !ok {
		return
	}
	if measured == 0 {
		ev.skipPgoLane(evidence, "no representative seed workloads available for measurement")
		return
	}
	evidence.PgoComparisons = comparisons
	evidence.PgoNote = fmt.Sprintf("informational PGO comparison over %d representative workload(s), %d A/B pairs each, lane cost %s; both sides built with -pgo=%s from the discovery CPU profile; this lane never changes accept/reject decisions", measured, s.pairs, ev.now().Sub(started).Round(time.Second), filepath.Base(ev.pgoProfilePath))
	_ = ev.journal.event("pgo_lane_completed", evidence.PgoNote, nil)
}

// pgoBuildBudget is the wall-clock bound on one profile-guided build in the
// informational lane.
func (ev *evaluator) pgoBuildBudget() time.Duration {
	if ev.pgoBuildTimeout > 0 {
		return ev.pgoBuildTimeout
	}
	return pgoLaneBuildTimeout
}

// pgoLaneUnaffordable reports why the informational lane should not start, or
// an empty string when the campaign can afford it. A campaign with no duration
// bound has nothing to protect.
func (ev *evaluator) pgoLaneUnaffordable() string {
	budget := ev.manifest.Campaign.MaxDuration.Duration()
	if budget <= 0 {
		return ""
	}
	floor := 3 * ev.pgoBuildBudget()
	remaining := budget - ev.elapsed()
	if remaining >= floor {
		return ""
	}
	return fmt.Sprintf("campaign has %s of its %s budget left and the lane may spend up to %s", remaining.Round(time.Second), budget, floor)
}

func (ev *evaluator) skipPgoLane(evidence *orchestrator.CandidateEvidence, reason string) {
	evidence.PgoNote = "PGO lane skipped: " + reason
	_ = ev.journal.event("pgo_lane_skipped", evidence.PgoNote, nil)
}

func (ev *evaluator) pgoProfileReady() (bool, string) {
	if ev.pgoProfilePath == "" {
		return false, "no pprof-format CPU profile was collected during discovery (sampler reports are not pprof and cannot seed -pgo)"
	}
	if info, err := os.Stat(ev.pgoProfilePath); err != nil || info.IsDir() || info.Size() == 0 {
		return false, fmt.Sprintf("discovery CPU profile %q missing or empty on disk", ev.pgoProfilePath)
	}
	return true, ""
}

func (ev *evaluator) buildPgoBinaries(ctx context.Context, candidateWorktree, candidateID string, evidence *orchestrator.CandidateEvidence) (baselinePgo, candidatePgo string, ok bool) {
	binDir := filepath.Join(ev.dir, "builds")
	binarySuffix := filepath.Base(ev.manifest.Target.Build.Binary)
	baselinePgo = filepath.Join(binDir, candidateID+"-baseline-pgo-"+binarySuffix)
	candidatePgo = filepath.Join(binDir, candidateID+"-candidate-pgo-"+binarySuffix)
	for _, target := range []struct {
		label, repository, output string
	}{
		{"baseline-pgo", ev.repository, baselinePgo},
		{"candidate-pgo", candidateWorktree, candidatePgo},
	} {
		if !ev.buildPgoBinary(ctx, target.label, target.repository, target.output, evidence) {
			return "", "", false
		}
	}
	return baselinePgo, candidatePgo, true
}

func (ev *evaluator) buildPgoBinary(ctx context.Context, label, repository, output string, evidence *orchestrator.CandidateEvidence) bool {
	result, err := ev.toolchain.Build(ctx, toolchain.BuildRequest{Repository: repository, Directory: ev.manifest.Target.Build.Directory, Target: ev.manifest.Target.Build.Package, Output: output, PGOProfile: ev.pgoProfilePath, Env: []string{"GOTOOLCHAIN=local"}})
	if err != nil {
		// A build the lane's own budget cut short is a statement about the
		// lane, not about the target's compiler output.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ev.skipPgoLane(evidence, fmt.Sprintf("%s build exceeded the lane's %s budget", label, ev.pgoBuildBudget()))
			return false
		}
		detail := tail(string(result.Stderr), 200)
		if detail == "" {
			detail = err.Error()
		}
		ev.skipPgoLane(evidence, fmt.Sprintf("%s build failed: %s", label, detail))
		return false
	}
	evidence.ArtifactURIs = append(evidence.ArtifactURIs, output)
	return true
}

func (ev *evaluator) measurePgoWorkloads(ctx context.Context, s evalSettings, evidence *orchestrator.CandidateEvidence, candidateID, baselinePgo, candidatePgo string) ([]domain.MetricComparison, int, bool) {
	comparisons := make([]domain.MetricComparison, 0, 3)
	measured := 0
	for _, seed := range ev.manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		baseReq := seedMeasurementRequest(ev.manifest, ev.campaignID, seed, candidateID+"-baseline-pgo", baselinePgo)
		candReq := baseReq
		candReq.Build = runner.Build{ID: candidateID + "-candidate-pgo", BinaryPath: candidatePgo}
		candReq.Workload.Command.Path = candidatePgo
		ab, err := ev.runner.RunInterleaved(ctx, runner.ABRequest{Baseline: baseReq, Candidate: candReq, Repetitions: s.pairs})
		if err != nil {
			ev.skipPgoLane(evidence, fmt.Sprintf("measurement failed on workload %q: %v", seed.ID, err))
			return nil, 0, false
		}
		if ok, rep := pgoBehaviorOK(ab); !ok {
			ev.skipPgoLane(evidence, fmt.Sprintf("PGO-built binaries diverged on workload %q at repetition %d", seed.ID, rep))
			return nil, 0, false
		}
		comparisons = append(comparisons, compareMetric(seed.ID, "wall_time_ns", "ns", ev.regressionLimit("wall_time_ns"), ab.Baseline, ab.Candidate, wallTime)...)
		comparisons = append(comparisons, compareMetric(seed.ID, "cpu_time_ns", "ns", ev.regressionLimit("cpu_time_ns"), ab.Baseline, ab.Candidate, cpuTime)...)
		comparisons = append(comparisons, compareMetric(seed.ID, "peak_memory_bytes", "bytes", ev.regressionLimit("peak_memory_bytes"), ab.Baseline, ab.Candidate, peakMemory)...)
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
// workload; policy treats any regressed guardrail as a rejection. limit is the
// regression ceiling the metric is judged by, which bounds the equivalence
// branch of metricSupport (see regressionLimit).
func compareMetric(workload, metric, unit string, limit float64, baseline, candidateRuns []domain.RunResult, selector metricSelector) []domain.MetricComparison {
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
	result.StatisticallyFit = metricSupport(baseVals, candVals, result.Baseline, result.DeltaPercent, limit)
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

// regressionLimit is the ceiling a metric is judged by: the manifest's
// maximum_guardrail_regression_percent, narrowed or widened for one metric by
// a trade-off preset or an --allow, since both land in that guardrail's own
// limit. It bounds the equivalence branch of metricSupport.
func (ev *evaluator) regressionLimit(metric string) float64 {
	return policy.RegressionLimit(policyConfigFromManifest(ev.manifest), metric)
}

// welchSignificance is the |t| a difference must exceed to count, about
// p < 0.05 two-sided at these sample counts. Statistical support and
// significance both start from it.
const welchSignificance = 2.2

// metricSupport answers the question policy actually asks: "are these
// samples strong evidence about the candidate's effect?" Two things count as
// evidence. A significant difference (Welch's |t| above welchSignificance) is
// evidence of whatever the samples show. A reading that is not an improvement
// is also supported when the 95% confidence interval of its relative delta
// lies entirely below the metric's regression limit, i.e. the data confidently
// rules out a material regression even though no significant change was
// detected; that matters for jittery metrics like peak memory, where a
// genuinely flat candidate otherwise reads as unsupported.
//
// An improvement gets no equivalence support: ruling out a regression past the
// limit says nothing about whether a win is real. Before this split the branch
// admitted any win whose interval happened to fall under the limit, so without
// benchstat a 3.50% wall-time reading from a series with no detectable
// difference (Welch |t| 1.3) was accepted on the t-test fallback.
func metricSupport(a, b []float64, baselineMean, deltaPercent, limit float64) bool {
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
	if t > welchSignificance {
		return true
	}
	if deltaPercent < 0 {
		return false // an improvement needs significance, not equivalence
	}
	if baselineMean <= 0 || deltaPercent >= limit {
		return false
	}
	ciUpper := ((mb - ma) + welchSignificance*se) / baselineMean * 100
	return ciUpper < limit
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
	return t > welchSignificance
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
