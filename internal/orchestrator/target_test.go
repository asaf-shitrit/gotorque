package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
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

// inputRecorder is an optimizer that remembers the text of every input it was
// handed and answers with a fixed proposal.
func inputRecorder(t *testing.T, inputs *[]string) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "optimizer",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				var text strings.Builder
				if content := ctx.UserContent(); content != nil {
					for _, part := range content.Parts {
						text.WriteString(part.Text)
					}
				}
				*inputs = append(*inputs, text.String())
				ev := session.NewEvent(ctx, ctx.InvocationID())
				ev.Output = map[string]any{"hypothesis": "buffer output", "patch": []string{"diff"}}
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("create optimizer: %v", err)
	}
	return a
}

// TestTheOptimizerReadsOnlyItsBrief: with a code-chosen target the optimizer
// sees the target, its excerpt, the policy and the earlier attempts, and none
// of the discovery evidence or analysis it is told not to act on. Once the
// targets run out it chooses for itself, so it gets the full state again.
func TestTheOptimizerReadsOnlyItsBrief(t *testing.T) {
	var calls int
	var inputs []string
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{}, &calls),
		Optimizer:   inputRecorder(t, &inputs),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{}, &calls),
	}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop}}}
	orch := mustNew(t, Dependencies{Runner: &hotRunner{}, Policy: &targetPolicy{}, Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-brief", causeCampaign, "finalize_campaign")

	if len(inputs) != 2 {
		t.Fatalf("optimizer called %d times, want 2", len(inputs))
	}
	var brief map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputs[0]), &brief); err != nil {
		t.Fatalf("first input is not JSON: %v\n%s", err, inputs[0])
	}
	want := []string{"optimization_mode", "target"}
	for _, key := range want {
		if _, ok := brief[key]; !ok {
			t.Errorf("brief lacks %q: %s", key, inputs[0])
		}
	}
	for _, key := range []string{"discovery", "analysis", "inspection", "explorer", "request"} {
		if _, ok := brief[key]; ok {
			t.Errorf("brief carries %q, which the optimizer is not to act on", key)
		}
	}
	var second map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputs[1]), &second); err != nil {
		t.Fatalf("second input is not JSON: %v", err)
	}
	if _, ok := second["discovery"]; !ok {
		t.Errorf("with no target left the optimizer should see the full state, got keys %v", slices.Collect(maps.Keys(second)))
	}
	if _, ok := second["prior_candidates"]; !ok {
		t.Errorf("second input lacks the first attempt")
	}
}
