package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

type fakeCauseAnalyst struct {
	requests []CauseRequest
	result   agents.AnalystResult
	err      error
}

func (f *fakeCauseAnalyst) AnalyzeCauses(_ context.Context, req CauseRequest) (agents.AnalystResult, error) {
	f.requests = append(f.requests, req)
	return f.result, f.err
}

// hotRunner reports measured hot functions from discovery and remembers the
// analysis each candidate was evaluated with.
type hotRunner struct {
	fakeRunnerService
	analyses []agents.AnalystResult
}

func (r *hotRunner) Discover(ctx context.Context, req DiscoveryRequest) (DiscoveryEvidence, error) {
	evidence, err := r.fakeRunnerService.Discover(ctx, req)
	evidence.HotFunctions = []string{"main.go:207"}
	return evidence, err
}

func (r *hotRunner) EvaluateCandidate(ctx context.Context, req CandidateRequest) (CandidateEvidence, error) {
	r.analyses = append(r.analyses, req.Analysis)
	return r.fakeRunnerService.EvaluateCandidate(ctx, req)
}

func causeGraph(t *testing.T, analyst *fakeCauseAnalyst, analystCalls *int) (*Orchestrator, *hotRunner, *fakeJobService) {
	t.Helper()
	var calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "objective", NextExperiment: "experiment"}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"from the model"}}, analystCalls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "diff"}, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: true}, &calls),
	}
	runner := &hotRunner{}
	jobs := &fakeJobService{}
	orch := mustNew(t, Dependencies{
		Runner: runner,
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
		Jobs:   jobs,
		Agents: roleSet,
		Causes: analyst,
	}, Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	return orch, runner, jobs
}

var causeCampaign = CampaignRequest{
	CampaignID:       "campaign-causes",
	Repository:       "/repo",
	BaseRevision:     "abc123",
	BuildTarget:      "./cmd/tool",
	OptimizationMode: domain.PolicyIdiomatic,
}

// TestCauseAnalystReplacesTheAnalystAgent: with a cause analyst configured the
// analyst model is never called, the analyst sees discovery's hot functions,
// and its result is the analysis every candidate is evaluated with.
func TestCauseAnalystReplacesTheAnalystAgent(t *testing.T) {
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{
		HotPaths:            []agents.HotPath{{Location: "main.go:207"}},
		CandidateHypotheses: []string{"buffer the per-statement writes"},
	}}
	var analystAgentCalls int
	orch, runner, _ := causeGraph(t, analyst, &analystAgentCalls)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-causes", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 2 {
		t.Fatalf("candidates tried = %d, want 2", result.CandidatesTried)
	}
	if analystAgentCalls != 0 {
		t.Errorf("analyst model called %d times, want never", analystAgentCalls)
	}
	if len(analyst.requests) != 2 {
		t.Fatalf("cause analyst calls = %d, want one per cycle", len(analyst.requests))
	}
	if got := analyst.requests[0]; got.Campaign.Repository != "/repo" || !slices.Equal(got.Discovery.HotFunctions, []string{"main.go:207"}) {
		t.Errorf("cause request = %+v, want the campaign repository and discovery's hot functions", got)
	}
	for i, analysis := range runner.analyses {
		if !slices.Equal(analysis.CandidateHypotheses, []string{"buffer the per-statement writes"}) {
			t.Errorf("candidate %d evaluated with analysis %+v, want the cause analyst's", i+1, analysis)
		}
	}
}

// TestFailingCauseAnalystDegradesLikeTheAgent: a gateway that refuses every
// request costs the analysis, not the campaign, and the cause is recorded
// against the analyst role.
func TestFailingCauseAnalystDegradesLikeTheAgent(t *testing.T) {
	analyst := &fakeCauseAnalyst{err: errors.New("gateway returned HTTP 429 for Jev")}
	var analystAgentCalls int
	orch, runner, jobs := causeGraph(t, analyst, &analystAgentCalls)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-causes-degraded", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 2 {
		t.Errorf("candidates tried = %d, want the campaign to continue", result.CandidatesTried)
	}
	if len(jobs.degraded) != 2 || jobs.degraded[0].role != "analyst" || jobs.degraded[0].cause != "gateway returned HTTP 429 for Jev" {
		t.Errorf("degraded = %+v, want the analyst's error recorded each cycle", jobs.degraded)
	}
	if len(runner.analyses) != 2 || len(runner.analyses[0].CandidateHypotheses) != 0 {
		t.Errorf("analyses = %+v, want the empty degraded result", runner.analyses)
	}
}
