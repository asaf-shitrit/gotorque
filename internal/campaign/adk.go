package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/policy"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// RunADK executes the bounded ADK graph against this persisted campaign. The
// deterministic services below are adapters to the same engine state and
// artifact store used by the CLI path; they do not provide shell access to
// agents. A caller may inject OpenAI-backed or static agents.
func (e *Engine) RunADK(ctx context.Context, roleSet agents.Set, cfg orchestrator.Config) (orchestrator.CampaignResult, error) {
	// Token usage is accounting, not a verdict, so it is recorded on the way out
	// whatever happened. A campaign cut short by its budget, or by a provider
	// that stalled until the deadline, spent real money, and this per-role table
	// is the only place that reports it: snapshotting it only after a clean ADK
	// result meant a budget-spent campaign — exactly the outcome the patch
	// budget is meant to reach — reported no cost at all.
	defer e.recordTokenUsage(roleSet)
	cfg.StopWhenRankingExhausted = !e.state.FreeChoice
	adk, message, err := e.prepareADK(roleSet, cfg)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	result, err := collectADKResult(ctx, adk, e.state.ID, message)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	if result.CampaignID == "" {
		return orchestrator.CampaignResult{}, errors.New("ADK completed without a campaign result")
	}
	_ = e.saveEvent("adk_completed", result.StopReason, result)
	return result, nil
}

// recordTokenUsage folds the collector's cumulative per-role totals for this
// process on top of whatever was persisted before this process's ADK work
// began. Collectors are created per process (NewOpenAIProviderFromEnvironment
// -> NewUsageCollector), so roleSet.Usage.Snapshot alone only ever reports
// this process's spend; a resumed campaign that already carried TokenUsage
// from an earlier process would otherwise have it overwritten by whatever the
// new process spent, silently discarding the old numbers.
//
// tokenUsageBaseline is captured once, lazily, from state.TokenUsage as it
// stood the first time this runs in the process -- before this process has
// written anything of its own. Every later call (RunADK's deferred call, or
// the refresh saveEvent makes while an ADK run is active) recomputes the same
// baseline+snapshot sum from scratch rather than adding a delta, so calling
// it any number of times, in any order, across any number of RunADK calls in
// one process, never double-counts: the collector's Snapshot is already
// cumulative since the collector was created.
func (e *Engine) recordTokenUsage(roleSet agents.Set) {
	if roleSet.Usage == nil {
		return
	}
	if e.tokenUsageBaseline == nil {
		e.tokenUsageBaseline = cloneTokenUsage(e.state.TokenUsage)
	}
	e.state.TokenUsage = mergeTokenUsage(e.tokenUsageBaseline, snapshotTokenUsage(roleSet.Usage.Snapshot()))
}

// cloneTokenUsage copies a persisted usage snapshot so later mutation of
// state.TokenUsage cannot reach back and change the baseline it was computed
// from. It always returns a non-nil map so callers can use nilness as "no
// baseline captured yet" without confusing it with "captured an empty one".
func cloneTokenUsage(usage map[string]RoleUsageSnapshot) map[string]RoleUsageSnapshot {
	clone := make(map[string]RoleUsageSnapshot, len(usage))
	for role, u := range usage {
		clone[role] = u
	}
	return clone
}

// mergeTokenUsage adds this process's cumulative-so-far usage on top of the
// baseline persisted before that process started, per role. processTotal is
// itself cumulative since the collector was created, not an increment, which
// is what makes calling this repeatedly with a fixed base idempotent.
func mergeTokenUsage(base, processTotal map[string]RoleUsageSnapshot) map[string]RoleUsageSnapshot {
	merged := make(map[string]RoleUsageSnapshot, len(base)+len(processTotal))
	for role, u := range base {
		merged[role] = u
	}
	for role, p := range processTotal {
		u := merged[role]
		u.Requests += p.Requests
		u.PromptTokens += p.PromptTokens
		u.CompletionTokens += p.CompletionTokens
		u.TotalTokens += p.TotalTokens
		merged[role] = u
	}
	return merged
}

func (e *Engine) prepareADK(roleSet agents.Set, cfg orchestrator.Config) (*adkrunner.Runner, *genai.Content, error) {
	if e.state.Status != StatusCompleted && e.state.Status != StatusRunning {
		return nil, nil, fmt.Errorf("campaign must be running or baseline-completed before ADK: %s", e.state.Status)
	}
	services := adkServices{engine: e}
	deps := orchestrator.Dependencies{Runner: services, Settler: services, Jobs: services, Agents: roleSet}
	if e.causeJev != nil {
		deps.Causes = causeAnalyst{engine: e, evaluator: e.causeJev, usage: roleSet.Usage}
	}
	if e.reviewJev != nil {
		deps.Review = reviewAnalyst{engine: e, evaluator: e.reviewJev, usage: roleSet.Usage}
	}
	orch, err := orchestrator.New(deps, cfg)
	if err != nil {
		return nil, nil, err
	}
	adk, err := adkrunner.NewInMemory("gotorque", orch.Agent)
	if err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(e.campaignRequest())
	if err != nil {
		return nil, nil, err
	}
	return adk, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: string(payload)}}}, nil
}

// campaignRequest builds the immutable campaign context every ADK node sees,
// including the analyst (as CauseRequest.Campaign; see causes.go). Objective
// is the manifest's performance primary metric after the campaign's
// trade-off was applied (ADR 0020), so a campaign started with --tradeoff
// lean carries "peak_memory_bytes" here and causeAnalyst.AnalyzeCauses ranks
// allocation causes first (ADR 0024).
func (e *Engine) campaignRequest() orchestrator.CampaignRequest {
	return orchestrator.CampaignRequest{
		CampaignID: e.state.ID, Repository: e.state.Repository, BaseRevision: e.state.Environment.Revision,
		BuildTarget: e.state.Manifest.Target.Build.Package, CommandArgs: append([]string(nil), e.state.Manifest.Target.Command...),
		OptimizationMode: e.state.Manifest.OptimizationPolicy, PriorTargets: append([]agents.Target(nil), e.state.HistoryTargets...),
		RecordedCandidates: recordedCandidates(e.state.CandidateRecords),
		Objective:          e.state.Manifest.Performance.PrimaryMetric, EarlierCandidates: e.state.HistoryPriors, GoVersion: goDirective(e.state.Repository, e.state.Manifest.Target.Build.Directory),
	}
}

// recordedCandidates hands the graph this campaign's persisted verdicts, so a
// resumed campaign continues its candidate count, attempt numbers and tried
// targets, and the optimizer reads the same history it would have read
// without the interruption, including the one more attempt an unmeasured
// target gets (ADR 0017).
func recordedCandidates(records []CandidateRecord) []orchestrator.PriorCandidate {
	out := make([]orchestrator.PriorCandidate, 0, len(records))
	for _, r := range records {
		out = append(out, orchestrator.PriorCandidate{
			Attempt: r.Attempt, Hypothesis: r.Hypothesis, Decision: string(r.Decision), Reasons: r.Reasons,
			FailureDetail: r.FailureDetail, Target: r.Target, ReviewConcerns: r.ReviewConcerns, Unmeasured: r.Unmeasured,
		})
	}
	return out
}

func collectADKResult(ctx context.Context, adk *adkrunner.Runner, sessionID string, message *genai.Content) (orchestrator.CampaignResult, error) {
	var result orchestrator.CampaignResult
	for event, runErr := range adk.Run(ctx, "gotorque", sessionID, message, adkagent.RunConfig{}) {
		if runErr != nil {
			return orchestrator.CampaignResult{}, runErr
		}
		if !isFinalizeCampaign(event) {
			continue
		}
		decoded, err := decodeCampaignResult(event.Output)
		if err != nil {
			return orchestrator.CampaignResult{}, err
		}
		result = decoded
	}
	return result, nil
}

func isFinalizeCampaign(event *session.Event) bool {
	return event != nil && event.Output != nil && event.NodeInfo != nil && strings.Contains(event.NodeInfo.Path, "finalize_campaign")
}

func decodeCampaignResult(output any) (orchestrator.CampaignResult, error) {
	data, err := json.Marshal(output)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	var result orchestrator.CampaignResult
	err = json.Unmarshal(data, &result)
	return result, err
}

// snapshotTokenUsage converts the agents collector's per-role totals into the
// persisted campaign-state shape.
func snapshotTokenUsage(usage map[string]agents.RoleUsage) map[string]RoleUsageSnapshot {
	snapshots := make(map[string]RoleUsageSnapshot, len(usage))
	for role, u := range usage {
		snapshots[role] = RoleUsageSnapshot{Requests: u.Requests, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	}
	return snapshots
}

type adkServices struct{ engine *Engine }

// excerptCandidates orders the locations worth reading source for: the
// analyst's hot paths first, then discovery's own resolved positions.
//
// The analyst is a model asked to echo locations, and it reformats them --
// "compiler (line 121)", "query.go (file-level)" -- which parseLocation
// rejects, leaving the optimizer with no source and reduced to guessing patch
// context. Discovery already resolved those functions to path:line
// deterministically, so appending them keeps excerpts working regardless of
// how the analyst chose to phrase its answer.
func excerptCandidates(hotPaths []agents.HotPath, discovered []string) []agents.HotPath {
	candidates := append([]agents.HotPath(nil), hotPaths...)
	for _, location := range discovered {
		candidates = append(candidates, agents.HotPath{Location: location, Evidence: "measured during discovery"})
	}
	return candidates
}

// CollectExcerpts implements the optional orchestrator.ExcerptCollector
// capability, attaching real source windows around analyst hot paths.
func (s adkServices) CollectExcerpts(_ context.Context, analysis agents.AnalystResult) ([]orchestrator.SourceExcerpt, error) {
	candidates := excerptCandidates(analysis.HotPaths, s.engine.state.DiscoveryHotFunctions)
	excerpts, err := extractExcerpts(s.engine.state.Repository, candidates, defaultMaxExcerpts)
	excerpts = withFileHeaders(s.engine.state.Repository, excerpts)
	locs := make([]string, 0, len(analysis.HotPaths))
	for _, hp := range analysis.HotPaths {
		locs = append(locs, hp.Location)
	}
	detail := fmt.Sprintf("hot_paths=%d locations=%v candidates=%d excerpts=%d err=%v", len(analysis.HotPaths), locs, len(candidates), len(excerpts), err)
	_ = s.engine.saveEvent("excerpts_debug", detail, nil)
	return excerpts, err
}

func (s adkServices) StartCampaign(_ context.Context, req orchestrator.CampaignRequest) (domain.Job, error) {
	now := time.Now().UTC()
	job := domain.Job{ID: "job-" + req.CampaignID, Kind: "optimization_campaign", Status: domain.JobRunning, CreatedAt: now, UpdatedAt: now}
	_ = s.engine.saveEvent("adk_started", "ADK workflow started", req)
	return job, nil
}

// RoleDegradation records one agent node that failed and was absorbed.
type RoleDegradation struct {
	Role  string `json:"role"`
	Cause string `json:"cause"`
}

// RecordRoleDegraded persists a role whose model call failed while the graph
// absorbed the failure: the node continued with an empty result, so a candidate
// may be missing that role's output. Without this the cause reached only the
// process's stderr, which no report and no API consumer can read, and an
// operator saw a candidate explained as "patch is empty". The event also puts
// the cause on the campaign's progress stream, which is the writer the campaign
// was given rather than the one the process happens to have.
func (s adkServices) RecordRoleDegraded(_ context.Context, role string, cause error) error {
	reason := "unknown cause"
	if cause != nil {
		reason = cause.Error()
	}
	s.engine.state.DegradedRoles = append(s.engine.state.DegradedRoles, RoleDegradation{Role: role, Cause: reason})
	return s.engine.saveEvent("role_degraded", fmt.Sprintf("%s node failed, continuing with an empty result: %s", role, reason), nil)
}

// RoleRepair records one role output that parsed only after the decoder
// rewrote it.
type RoleRepair struct {
	Role   string `json:"role"`
	Repair string `json:"repair"`
}

// RecordRoleRepaired persists a role whose output parsed only after a repair.
// The repaired value is used as the role's answer, which is the point of the
// repair, but it used to leave no trace: an optimizer patch cut off at the
// output-token cap was closed by the decoder, had its hunk counts recomputed by
// normalization, and read in every record like a patch the model meant. The
// event marks it on the progress stream; the optimizer's repair also reaches
// its candidate record through the evidence.
func (s adkServices) RecordRoleRepaired(_ context.Context, role string, repair agents.Repair) error {
	message := fmt.Sprintf("%s output parsed only after the decoder %s", role, repair)
	return s.engine.saveEvent("role_repaired", message, RoleRepair{Role: role, Repair: string(repair)})
}

func (s adkServices) CompleteCampaign(_ context.Context, job domain.Job, result orchestrator.CampaignResult) (domain.Job, error) {
	job.Status = domain.JobSucceeded
	if result.ProviderFailure != "" {
		// The graph stopped because nothing answered, not because a bound was
		// reached, so the job did not succeed.
		job.Status = domain.JobFailed
	}
	job.UpdatedAt = time.Now().UTC()
	_ = s.engine.saveEvent("adk_finalized", result.StopReason, result)
	return job, nil
}
func (s adkServices) Discover(_ context.Context, _ orchestrator.DiscoveryRequest) (orchestrator.DiscoveryEvidence, error) {
	runs := make([]string, 0, len(s.engine.state.Runs))
	for _, run := range s.engine.state.Runs {
		runs = append(runs, run.ID)
	}
	hotFunctions := append([]string(nil), s.engine.state.DiscoveryHotFunctions...)
	metadata := map[string]string{}
	if s.engine.state.DiscoveryProfileSummaryPath != "" {
		metadata["profile_summary"] = s.engine.state.DiscoveryProfileSummaryPath
	}
	if len(s.engine.state.DiscoveryWorkloads) > 0 {
		metadata["explored_workloads"] = strings.Join(s.engine.state.DiscoveryWorkloads, "; ")
	}
	return orchestrator.DiscoveryEvidence{RunIDs: runs, CoveredPaths: hotFunctions, HotFunctions: hotFunctions, ProfileSummaryPath: s.engine.state.DiscoveryProfileSummaryPath, Summary: "baseline discovery evidence", Metadata: metadata, HotFunctionWeights: s.engine.state.DiscoveryHotFunctionWeights, UnbufferedWrites: s.engine.state.DiscoveryUnbufferedWrites}, nil
}

// Assess evaluates one proposal and judges the evidence with the campaign's
// acceptance policy. The verdict is computed here, from measurements and the
// behavior gate alone: the reviewer has not run, and policyVerdict reads no
// review.
func (s adkServices) Assess(ctx context.Context, req orchestrator.CandidateRequest) (orchestrator.Assessment, error) {
	evidence, err := s.engine.evaluateCandidate(ctx, req)
	if err != nil {
		return orchestrator.Assessment{}, err
	}
	return orchestrator.Assessment{Evidence: evidence, Verdict: s.engine.judge(evidence)}, nil
}

// judge is the acceptance policy's verdict on one candidate's evidence, as the
// graph carries it.
func (e *Engine) judge(evidence orchestrator.CandidateEvidence) domain.Evaluation {
	result := e.policyVerdict(evidence)
	return domain.Evaluation{CandidateID: evidence.Candidate.ID, Decision: result.Decision, BehaviorMatches: evidence.BehaviorMatches, Comparisons: result.Comparisons, Reasons: result.Reasons}
}

// Settle makes one candidate's verdict durable, in an order that cannot leave
// a record without its promotion:
//
//  1. An accepted candidate's patch is copied to accepted/ first. Nothing
//     refers to the copy yet, so a failure or a crash here leaves an orphan
//     file and no record; the attempt is simply evaluated again on resume.
//  2. The verdict joins the campaign's records, carrying its accepted marker,
//     in one saved state: candidate_evaluated, then the report snapshot. A
//     record on disk therefore always has its artifact, and an operator
//     reading the live report sees the accepted marker with the verdict.
//  3. candidate_accepted marks the promotion on the event stream.
//  4. The tallies are persisted last (adk_progress), after the record they
//     count, so a bound never counts a verdict that is not on disk.
//
// The verdict is recorded as given. It is not recomputed here.
func (s adkServices) Settle(_ context.Context, settlement orchestrator.Settlement) error {
	return s.engine.settle(settlement)
}

func (e *Engine) settle(settlement orchestrator.Settlement) error {
	evidence, verdict := settlement.Assessment.Evidence, settlement.Assessment.Verdict
	accepted := verdict.Decision == domain.DecisionAccepted
	if accepted {
		if err := e.keepAcceptedPatch(evidence.Candidate); err != nil {
			return fmt.Errorf("promote candidate: %w", err)
		}
	}
	// A failure to persist the verdict must not fail the settlement: the graph
	// would stop on it, and the verdict is already decided. The events after it
	// save the same state, so a store that is really gone fails there.
	_ = e.persistVerdict(len(e.state.CandidateRecords)+1, evidence, settlement.Target, settlement.Review, policy.Result{Decision: verdict.Decision, Comparisons: verdict.Comparisons, Reasons: verdict.Reasons}, accepted)
	if accepted {
		if err := e.saveEvent("candidate_accepted", "policy accepted candidate "+evidence.Candidate.ID, evidence.Candidate); err != nil {
			return err
		}
	}
	// The orchestrator owns the tallies; persisting them with every verdict is
	// what lets a later process resume the bounds instead of restarting them.
	e.state.ConsecutiveFailures = settlement.Progress.ConsecutiveFailures
	e.state.ConsecutiveInconclusive = settlement.Progress.ConsecutiveInconclusive
	return e.saveEvent("adk_progress", "ADK policy decision", settlement.Progress)
}

// keepAcceptedPatch copies an accepted candidate's patch into the campaign's
// accepted/ directory.
func (e *Engine) keepAcceptedPatch(candidate domain.Candidate) error {
	acceptedDir := filepath.Join(e.dir, "accepted")
	if err := os.MkdirAll(acceptedDir, 0o700); err != nil {
		return err
	}
	if candidate.PatchPath == "" {
		return nil
	}
	data, err := os.ReadFile(candidate.PatchPath)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(acceptedDir, candidate.ID+".diff"), data, 0o600)
}

// policyConfigFromManifest builds the acceptance policy from the target's own
// manifest. The performance block used to be loaded, validated, and then
// ignored, because Evaluate was handed policy.DefaultConfig(): a target
// declaring a 5% floor or a narrower guardrail list was judged by 3% and the
// default guardrails instead. The manifest is the contract, so its values win
// and the defaults only fill in what it leaves out.
func policyConfigFromManifest(m manifest.Manifest) policy.Config {
	config := policy.DefaultConfig()
	performance := m.Performance
	if performance.PrimaryMetric != "" {
		config.PrimaryMetric = performance.PrimaryMetric
	}
	if performance.MinimumImprovementPercent > 0 {
		config.MinimumImprovementPercent = performance.MinimumImprovementPercent
	}
	if performance.MaximumGuardrailRegressionPercent > 0 {
		config.MaximumGuardrailRegressionPercent = performance.MaximumGuardrailRegressionPercent
	}
	if performance.StatisticalSupportRequired != nil {
		config.StatisticalSupportRequired = *performance.StatisticalSupportRequired
	}
	if len(performance.Guardrails) > 0 {
		config.Guardrails = make([]policy.Guardrail, 0, len(performance.Guardrails))
		for _, guardrail := range performance.Guardrails {
			config.Guardrails = append(config.Guardrails, policy.Guardrail{Name: guardrail.Name, MaximumRegressionPercent: guardrail.MaximumRegressionPercent, Required: guardrail.Required})
		}
	}
	return config
}

// eligibleReadings are the comparisons a verdict may rest on: every reading of
// the primary metric, pooled or per workload.
func eligibleReadings(config policy.Config, comparisons []domain.MetricComparison) []domain.MetricComparison {
	var eligible []domain.MetricComparison
	for _, c := range comparisons {
		if c.Metric == config.PrimaryMetric {
			eligible = append(eligible, c)
		}
	}
	return eligible
}

// recordVerdict judges one candidate's evidence with the campaign's policy and
// persists the verdict. The null-candidate loop reaches its verdicts here; the
// graph's candidates are judged by Assess and recorded by Settle, which share
// persistVerdict. The verdict is returned even when persisting it failed.
func (e *Engine) recordVerdict(attempt int, evidence orchestrator.CandidateEvidence, target *agents.Target, review agents.ReviewerResult) (policy.Result, error) {
	result := e.policyVerdict(evidence)
	return result, e.persistVerdict(attempt, evidence, target, review, result, false)
}

// persistVerdict records a verdict that was already reached. accepted marks the
// record of a candidate whose patch is already in accepted/.
func (e *Engine) persistVerdict(attempt int, evidence orchestrator.CandidateEvidence, target *agents.Target, review agents.ReviewerResult, result policy.Result, accepted bool) error {
	record := candidateRecord(attempt, evidence, target, review, result)
	record.Accepted = accepted
	e.state.CandidateRecords = append(e.state.CandidateRecords, record)
	err := e.saveEvent("candidate_evaluated", candidateEventSummary(record, e.state.Manifest.Performance.PrimaryMetric), record)
	e.snapshotReports()
	return err
}

// candidateRecord is the persisted verdict for one candidate.
func candidateRecord(attempt int, evidence orchestrator.CandidateEvidence, target *agents.Target, review agents.ReviewerResult, result policy.Result) CandidateRecord {
	return CandidateRecord{
		Attempt:          attempt,
		CandidateID:      evidence.Candidate.ID,
		Hypothesis:       evidence.Candidate.Hypothesis,
		Target:           target,
		ReviewConcerns:   review.Concerns,
		PatchPath:        evidence.Candidate.PatchPath,
		Transport:        evidence.Candidate.Transport,
		Summary:          evidence.Summary,
		Decision:         result.Decision,
		Reasons:          result.Reasons,
		Comparisons:      result.Comparisons,
		BenchstatOutput:  evidence.BenchstatOutput,
		Samples:          evidence.RepSamples,
		PgoComparisons:   evidence.PgoComparisons,
		PgoNote:          evidence.PgoNote,
		ProposalRepair:   string(evidence.ProposalRepair),
		FailureDetail:    evidence.FailureDetail,
		LoadAverages:     evidence.LoadAverages,
		LoadContended:    evidence.LoadContended,
		QuietWait:        evidence.QuietWait,
		QuietWaitExpired: evidence.QuietWaitExpired,
		DiscardedLoad:    evidence.DiscardedLoad,
		Unmeasured:       evidence.Unmeasured,
	}
}
