package orchestrator

import (
	"context"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

// proposalRecorder wraps fakeRunnerService and remembers every proposal the
// graph handed to EvaluateCandidate, so a test can check the orchestrator
// passed the optimizer's output through unchanged.
type proposalRecorder struct {
	fakeRunnerService
	proposals []agents.OptimizerResult
}

func (r *proposalRecorder) EvaluateCandidate(ctx context.Context, req CandidateRequest) (CandidateEvidence, error) {
	r.proposals = append(r.proposals, req.Proposal)
	return r.fakeRunnerService.EvaluateCandidate(ctx, req)
}

// TestGraphPassesFunctionSourceThroughWithNoTarget: ADR 0022 adds
// function_source and imports to OptimizerResult and teaches deterministic
// code in internal/campaign to build a diff from them, but the orchestrator
// graph itself must keep doing exactly what it did before: hand the
// optimizer's output to EvaluateCandidate unchanged, patch included, whether
// or not it carries a target. With no analysis targets, code picks no target,
// and an optimizer answering with function_source instead of a hand-written
// patch (the model may still do this even without a target) must reach the
// candidate request exactly as returned.
func TestGraphPassesFunctionSourceThroughWithNoTarget(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "o", NextExperiment: "e"}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{}, &calls),
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{
			Hypothesis:     "buffer output",
			FunctionSource: "func f() {}",
			Imports:        []string{"bufio"},
		}, &calls),
		Reviewer: staticAgent(t, "reviewer", agents.ReviewerResult{}, &calls),
	}
	runner := &proposalRecorder{}
	seq := &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}}
	orch := mustNew(t, Dependencies{Runner: runner, Policy: seq, Jobs: &fakeJobService{}, Agents: roleSet},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	req := CampaignRequest{CampaignID: "c", Repository: "/repo", BaseRevision: "abc", BuildTarget: "./cmd/tool", OptimizationMode: domain.PolicyIdiomatic}
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-fs", req, "finalize_campaign")

	if len(runner.proposals) != 1 {
		t.Fatalf("EvaluateCandidate called %d times, want 1", len(runner.proposals))
	}
	got := runner.proposals[0]
	if got.Patch != "" {
		t.Errorf("patch = %q, want empty: the optimizer never set it", got.Patch)
	}
	if got.FunctionSource != "func f() {}" {
		t.Errorf("function_source = %q, want it passed through unchanged", got.FunctionSource)
	}
	if len(got.Imports) != 1 || got.Imports[0] != "bufio" {
		t.Errorf("imports = %v, want [bufio] passed through unchanged", got.Imports)
	}
}

// TestGraphPassesPatchThroughWithATarget: with a code-chosen target the
// orchestrator still forwards whatever the optimizer answered with,
// including an ordinary patch; resolving it into a diff or a rejection is
// internal/campaign's job (ADR 0022), not the graph's.
func TestGraphPassesPatchThroughWithATarget(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{}, &calls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "h", Patch: "diff"}, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{}, &calls),
	}
	runner := &proposalRecorder{}
	policy := &targetPolicy{}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop}}}
	orch := mustNew(t, Dependencies{Runner: runner, Policy: policy, Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-fs-target", causeCampaign, "finalize_campaign")

	if len(runner.proposals) != 1 || runner.proposals[0].Patch != "diff" {
		t.Fatalf("proposals = %+v, want one carrying the hand-written patch", runner.proposals)
	}
}
