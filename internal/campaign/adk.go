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

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/policy"
	"example.com/gotorque/internal/workload"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/genai"
)

// RunADK executes the bounded ADK graph against this persisted campaign. The
// deterministic services below are adapters to the same engine state and
// artifact store used by the CLI path; they do not provide shell access to
// agents. A caller may inject OpenAI-backed or static agents.
func (e *Engine) RunADK(ctx context.Context, roleSet agents.Set, cfg orchestrator.Config) (orchestrator.CampaignResult, error) {
	if e.state.Status != StatusCompleted && e.state.Status != StatusRunning {
		return orchestrator.CampaignResult{}, fmt.Errorf("campaign must be running or baseline-completed before ADK: %s", e.state.Status)
	}
	services := adkServices{engine: e}
	orch, err := orchestrator.New(orchestrator.Dependencies{Runner: services, Policy: services, Jobs: services, Agents: roleSet}, cfg)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	adk, err := adkrunner.NewInMemory("gotorque", orch.Agent)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	req := orchestrator.CampaignRequest{CampaignID: e.state.ID, Repository: e.state.Repository, BaseRevision: e.state.Environment.Revision, BuildTarget: e.state.Manifest.Target.Build.Package, CommandArgs: append([]string(nil), e.state.Manifest.Target.Command...), OptimizationMode: e.state.Manifest.OptimizationPolicy}
	payload, err := json.Marshal(req)
	if err != nil {
		return orchestrator.CampaignResult{}, err
	}
	message := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: string(payload)}}}
	var result orchestrator.CampaignResult
	for event, runErr := range adk.Run(ctx, "gotorque", e.state.ID, message, adkagent.RunConfig{}) {
		if runErr != nil {
			return orchestrator.CampaignResult{}, runErr
		}
		if event == nil || event.Output == nil || event.NodeInfo == nil || !strings.Contains(event.NodeInfo.Path, "finalize_campaign") {
			continue
		}
		data, err := json.Marshal(event.Output)
		if err != nil {
			return orchestrator.CampaignResult{}, err
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return orchestrator.CampaignResult{}, err
		}
	}
	if result.CampaignID == "" {
		return orchestrator.CampaignResult{}, errors.New("ADK completed without a campaign result")
	}
	if roleSet.Usage != nil {
		e.state.TokenUsage = snapshotTokenUsage(roleSet.Usage.Snapshot())
	}
	_ = e.saveEvent("adk_completed", result.StopReason, result)
	return result, nil
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

// CollectExcerpts implements the optional orchestrator.ExcerptCollector
// capability, attaching real source windows around analyst hot paths.
func (s adkServices) CollectExcerpts(ctx context.Context, analysis agents.AnalystResult) ([]orchestrator.SourceExcerpt, error) {
	return extractExcerpts(s.engine.state.Repository, analysis.HotPaths, defaultMaxExcerpts)
}

func (s adkServices) StartCampaign(ctx context.Context, req orchestrator.CampaignRequest) (domain.Job, error) {
	now := time.Now().UTC()
	job := domain.Job{ID: "job-" + req.CampaignID, Kind: "optimization_campaign", Status: domain.JobRunning, CreatedAt: now, UpdatedAt: now}
	_ = s.engine.saveEvent("adk_started", "ADK workflow started", req)
	return job, nil
}
func (s adkServices) RecordProgress(_ context.Context, _ domain.Job, progress orchestrator.CampaignProgress) error {
	return s.engine.saveEvent("adk_progress", "ADK policy decision", progress)
}
func (s adkServices) CompleteCampaign(_ context.Context, job domain.Job, result orchestrator.CampaignResult) (domain.Job, error) {
	job.Status = domain.JobSucceeded
	job.UpdatedAt = time.Now().UTC()
	_ = s.engine.saveEvent("adk_finalized", result.StopReason, result)
	return job, nil
}
func (s adkServices) Inspect(_ context.Context, _ orchestrator.CampaignRequest) (orchestrator.Inspection, error) {
	return orchestrator.Inspection{Packages: append([]string(nil), s.engine.state.Inventory.Packages...), Commands: append([]string(nil), s.engine.state.Inventory.Commands...), Metadata: map[string]string{"authority": s.engine.state.Environment.Authority}}, nil
}
func (s adkServices) Discover(_ context.Context, req orchestrator.DiscoveryRequest) (orchestrator.DiscoveryEvidence, error) {
	runs := []string{}
	for _, run := range s.engine.state.Runs {
		runs = append(runs, run.ID)
	}
	hotFunctions := append([]string(nil), s.engine.state.DiscoveryHotFunctions...)
	metadata := map[string]string{"entry_points": fmt.Sprint(len(req.Explorer.EntryPoints))}
	if s.engine.state.DiscoveryProfileSummaryPath != "" {
		metadata["profile_summary"] = s.engine.state.DiscoveryProfileSummaryPath
	}
	// Explorer proposals are model output: validate each one deterministically
	// and drop invalid proposals instead of failing the whole turn, mirroring
	// the fixture-shape tolerance used elsewhere.
	accepted := 0
	var rejections []string
	for _, proposal := range req.Explorer.Proposals {
		if err := workload.ValidateProposal(proposal, s.engine.state.Manifest); err != nil {
			rejections = append(rejections, fmt.Sprintf("%s: %v", proposal.Name, err))
			continue
		}
		accepted++
	}
	metadata["proposals_accepted"] = fmt.Sprint(accepted)
	metadata["proposals_rejected"] = fmt.Sprint(len(rejections))
	if len(rejections) > 0 {
		metadata["proposal_rejections"] = strings.Join(rejections, "; ")
	}
	summary := fmt.Sprintf("baseline discovery evidence (%d/%d explorer proposals valid)", accepted, len(req.Explorer.Proposals))
	return orchestrator.DiscoveryEvidence{RunIDs: runs, CoveredPaths: hotFunctions, HotFunctions: hotFunctions, ProfileSummaryPath: s.engine.state.DiscoveryProfileSummaryPath, Summary: summary, Metadata: metadata}, nil
}
func (s adkServices) EvaluateCandidate(ctx context.Context, req orchestrator.CandidateRequest) (orchestrator.CandidateEvidence, error) {
	return s.engine.evaluateCandidate(ctx, req)
}

func (s adkServices) PromoteCandidate(_ context.Context, candidate domain.Candidate) error {
	acceptedDir := filepath.Join(s.engine.dir, "accepted")
	if err := os.MkdirAll(acceptedDir, 0o700); err != nil {
		return err
	}
	if candidate.PatchPath != "" {
		data, err := os.ReadFile(candidate.PatchPath)
		if err != nil {
			return err
		}
		acceptedPath := filepath.Join(acceptedDir, candidate.ID+".diff")
		if err := os.WriteFile(acceptedPath, data, 0o600); err != nil {
			return err
		}
	}
	for i := range s.engine.state.CandidateRecords {
		if s.engine.state.CandidateRecords[i].CandidateID == candidate.ID {
			s.engine.state.CandidateRecords[i].Accepted = true
		}
	}
	return s.engine.saveEvent("candidate_accepted", "policy accepted candidate "+candidate.ID, candidate)
}

func (s adkServices) Evaluate(_ context.Context, input orchestrator.PolicyInput) (domain.Evaluation, error) {
	comparisons := make([]policy.Comparison, 0, len(input.Evidence.Comparisons))
	for _, c := range input.Evidence.Comparisons {
		comparisons = append(comparisons, policy.Comparison{Name: c.Name, Unit: c.Unit, Baseline: c.Baseline, Candidate: c.Candidate, StatisticallySupported: c.StatisticallyFit})
	}
	result := policy.Evaluate(policy.DefaultConfig(), policy.Evidence{
		BehaviorMatches:        input.Evidence.BehaviorMatches,
		SafetyChecksPassed:     input.Evidence.SafetyChecksPassed,
		RepresentativeEvidence: input.Evidence.RepresentativeEvidence,
		Comparisons:            comparisons,
	})
	converted := make([]domain.MetricComparison, 0, len(result.Comparisons))
	for _, c := range result.Comparisons {
		converted = append(converted, domain.MetricComparison{Name: c.Name, Unit: c.Unit, Baseline: c.Baseline, Candidate: c.Candidate, DeltaPercent: c.DeltaPercent, StatisticallyFit: c.StatisticallySupported})
	}
	// Persist the full verdict so reports can explain every decision.
	record := CandidateRecord{
		Attempt:         len(s.engine.state.CandidateRecords) + 1,
		CandidateID:     input.Evidence.Candidate.ID,
		Hypothesis:      input.Evidence.Candidate.Hypothesis,
		PatchPath:       input.Evidence.Candidate.PatchPath,
		Summary:         input.Evidence.Summary,
		Decision:        result.Decision,
		Reasons:         result.Reasons,
		Comparisons:     converted,
		BenchstatOutput: input.Evidence.BenchstatOutput,
		PgoComparisons:  input.Evidence.PgoComparisons,
		PgoNote:         input.Evidence.PgoNote,
	}
	s.engine.state.CandidateRecords = append(s.engine.state.CandidateRecords, record)
	// Persist immediately: an ADK failure later in the run must not lose
	// already-evaluated verdicts from bbolt.
	_ = s.engine.saveEvent("candidate_evaluated", fmt.Sprintf("attempt %d: %s", record.Attempt, result.Decision), record)
	return domain.Evaluation{CandidateID: input.Evidence.Candidate.ID, Decision: result.Decision, BehaviorMatches: input.Evidence.BehaviorMatches, Comparisons: converted, Reasons: result.Reasons}, nil
}
