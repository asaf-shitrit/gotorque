package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
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

func causeGraph(t *testing.T, analyst *fakeCauseAnalyst) (*Orchestrator, *hotRunner, *fakeJobService) {
	t.Helper()
	var calls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "diff"}, &calls),
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

// TestCauseAnalystServesTheAnalystNode: the analyst sees discovery's hot
// functions, and its result is the analysis every candidate is evaluated with.
func TestCauseAnalystServesTheAnalystNode(t *testing.T) {
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{
		HotPaths:            []agents.HotPath{{Location: "main.go:207"}},
		CandidateHypotheses: []string{"buffer the per-statement writes"},
	}}
	orch, runner, _ := causeGraph(t, analyst)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-causes", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 2 {
		t.Fatalf("candidates tried = %d, want 2", result.CandidatesTried)
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

// TestFailingCauseAnalystDegradesLikeAnyRole: a gateway that refuses every
// request costs the analysis, not the campaign, and the cause is recorded
// against the analyst role.
func TestFailingCauseAnalystDegradesLikeAnyRole(t *testing.T) {
	analyst := &fakeCauseAnalyst{err: errors.New("gateway returned HTTP 429 for Jev")}
	orch, runner, jobs := causeGraph(t, analyst)
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

// exhaustionGraph is causeGraph with the ranking-exhaustion stop configurable
// and room for more candidates than the analysis has targets.
func exhaustionGraph(t *testing.T, analyst *fakeCauseAnalyst, stop bool) (*Orchestrator, *int) {
	t.Helper()
	var optimizerCalls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "diff"}, &optimizerCalls),
	}
	orch := mustNew(t, Dependencies{
		Runner: &hotRunner{},
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionInconclusive}},
		Jobs:   &fakeJobService{},
		Agents: roleSet,
		Causes: analyst,
	}, Config{MaxCandidates: 3, MaxConsecutiveFailures: 5, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1, StopWhenRankingExhausted: stop})
	return orch, &optimizerCalls
}

func oneTargetAnalyst() *fakeCauseAnalyst {
	return &fakeCauseAnalyst{result: agents.AnalystResult{
		HotPaths: []agents.HotPath{{Location: "main.go:207"}},
		Targets:  []agents.Target{{Location: "main.go:207", Function: "write", Cause: "unbuffered_io", Remedy: "buffer it"}},
	}}
}

// TestCampaignStopsWhenEveryFlaggedTargetWasTried: with the stop on, a
// campaign with one flagged target spends one candidate on it and then
// finishes, instead of handing the optimizer free choice.
func TestCampaignStopsWhenEveryFlaggedTargetWasTried(t *testing.T) {
	orch, optimizerCalls := exhaustionGraph(t, oneTargetAnalyst(), true)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-exhausted", causeCampaign, "finalize_campaign")
	if result.CandidatesTried != 1 {
		t.Fatalf("candidates tried = %d, want 1", result.CandidatesTried)
	}
	if result.StopReason != stopReasonRankingExhausted {
		t.Errorf("stop reason = %q, want %q", result.StopReason, stopReasonRankingExhausted)
	}
	if *optimizerCalls != 1 {
		t.Errorf("optimizer calls = %d, want 1", *optimizerCalls)
	}
}

// TestCampaignKeepsFreeChoiceWithoutTheStop pins the previous behavior when
// the stop is off (--free-choice).
func TestCampaignKeepsFreeChoiceWithoutTheStop(t *testing.T) {
	orch, _ := exhaustionGraph(t, oneTargetAnalyst(), false)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-free", causeCampaign, "finalize_campaign")
	if result.CandidatesTried != 3 {
		t.Fatalf("candidates tried = %d, want 3", result.CandidatesTried)
	}
	if result.StopReason != stopReasonMaxCandidates {
		t.Errorf("stop reason = %q, want %q", result.StopReason, stopReasonMaxCandidates)
	}
}

// TestCampaignStopsWhenTheAnalysisFlaggedNothing: with the stop on, an
// analysis that flags no target ends the campaign before any candidate, and
// says why.
func TestCampaignStopsWhenTheAnalysisFlaggedNothing(t *testing.T) {
	orch, optimizerCalls := exhaustionGraph(t, &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:207"}}}}, true)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-nothing", causeCampaign, "finalize_campaign")
	if result.CandidatesTried != 0 || *optimizerCalls != 0 {
		t.Fatalf("candidates = %d, optimizer calls = %d, want none", result.CandidatesTried, *optimizerCalls)
	}
	if result.StopReason != stopReasonNothingFlagged {
		t.Errorf("stop reason = %q, want %q", result.StopReason, stopReasonNothingFlagged)
	}
}
