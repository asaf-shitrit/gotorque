package orchestrator

import (
	"context"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

var (
	targetLoop  = agents.Target{Location: "main.go:206", Function: "gron", Cause: "unbuffered_io", Remedy: "Route the writes in gron through bufio.", Z: 3.3}
	targetAlloc = agents.Target{Location: "statements.go:26", Function: "(statement).String", Cause: "alloc", Remedy: "Remove the allocation in String.", Z: 1.5}
)

func excerptsFor(locations ...string) []SourceExcerpt {
	out := make([]SourceExcerpt, 0, len(locations))
	for _, l := range locations {
		out = append(out, SourceExcerpt{Path: l, HotPath: l, Content: "source of " + l})
	}
	return out
}

func TestPlanTargetPicksTheFirstUntriedTarget(t *testing.T) {
	state := CampaignState{
		Analysis:        agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
		SourceExcerpts:  excerptsFor("main.go:206", "statements.go:26", "token.go:141"),
		PriorCandidates: []PriorCandidate{{Attempt: 1, Target: &targetLoop}},
	}
	planTarget(&state)
	if state.Target == nil || *state.Target != targetAlloc {
		t.Fatalf("target = %+v, want the untried alloc target", state.Target)
	}
	if state.Coordinator.NextExperiment != targetAlloc.Remedy || state.Coordinator.Objective != targetObjective {
		t.Errorf("coordinator = %+v, want the target's remedy as the experiment", state.Coordinator)
	}
	if len(state.SourceExcerpts) != 1 || state.SourceExcerpts[0].HotPath != "statements.go:26" {
		t.Errorf("excerpts = %+v, want only the target's source", state.SourceExcerpts)
	}
}

// TestPlanTargetHonoursTargetsTriedBeforeAResume: the graph's own prior
// candidates restart empty on resume, so the persisted targets carry over.
func TestPlanTargetHonoursTargetsTriedBeforeAResume(t *testing.T) {
	state := CampaignState{
		Request:  CampaignRequest{PriorTargets: []agents.Target{targetLoop, targetAlloc}},
		Analysis: agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
	}
	planTarget(&state)
	if state.Target != nil {
		t.Errorf("target = %+v, want none once every target was tried", state.Target)
	}
}

func TestPlanTargetKeepsAllExcerptsWhenTheTargetHasNone(t *testing.T) {
	state := CampaignState{
		Analysis:       agents.AnalystResult{Targets: []agents.Target{targetLoop}},
		SourceExcerpts: excerptsFor("token.go:141", "identifier.go:52"),
	}
	planTarget(&state)
	if state.Target == nil || len(state.SourceExcerpts) != 2 {
		t.Errorf("target %+v with %d excerpts, want the target and every excerpt kept", state.Target, len(state.SourceExcerpts))
	}
}

// TestPlanTargetLeavesTheModelAnalystPathAlone: a model analyst ranks nothing,
// so its coordinator plan and excerpts pass through untouched.
func TestPlanTargetLeavesTheModelAnalystPathAlone(t *testing.T) {
	coordinator := agents.CoordinatorResult{Objective: "from the model", NextExperiment: "profile scan"}
	state := CampaignState{Coordinator: coordinator, SourceExcerpts: excerptsFor("a.go:1", "b.go:2"), Target: &targetLoop}
	planTarget(&state)
	if state.Target != nil || state.Coordinator.Objective != "from the model" || len(state.SourceExcerpts) != 2 {
		t.Errorf("state changed on the model path: target %+v, coordinator %+v, %d excerpts", state.Target, state.Coordinator, len(state.SourceExcerpts))
	}
}

// targetPolicy rejects every candidate and remembers the target each
// verdict was recorded with.
type targetPolicy struct{ targets []*agents.Target }

func (p *targetPolicy) Evaluate(_ context.Context, input PolicyInput) (domain.Evaluation, error) {
	p.targets = append(p.targets, input.Target)
	return domain.Evaluation{CandidateID: input.Evidence.Candidate.ID, Decision: domain.DecisionRejected}, nil
}

// TestCodeChoosesEachCycleTarget drives the graph for three cycles: the
// coordinator model is never called, each cycle attacks the next untried
// target, and once the targets run out the optimizer is left to choose.
func TestCodeChoosesEachCycleTarget(t *testing.T) {
	var coordinatorCalls, calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "from the model"}, &coordinatorCalls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{}, &calls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "diff"}, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{}, &calls),
	}
	policy := &targetPolicy{}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	orch := mustNew(t, Dependencies{Runner: &hotRunner{}, Policy: policy, Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 3, MaxConsecutiveFailures: 3, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-targets", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 3 {
		t.Fatalf("candidates tried = %d, want 3", result.CandidatesTried)
	}
	if coordinatorCalls != 0 {
		t.Errorf("coordinator model called %d times, want never", coordinatorCalls)
	}
	if len(policy.targets) != 3 || policy.targets[0] == nil || *policy.targets[0] != targetLoop ||
		policy.targets[1] == nil || *policy.targets[1] != targetAlloc || policy.targets[2] != nil {
		t.Errorf("targets per verdict = %v, want the loop, then the allocation, then none", policy.targets)
	}
}
