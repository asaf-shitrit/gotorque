package orchestrator

import (
	"context"
	"slices"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

var repairCampaign = CampaignRequest{
	CampaignID:       "campaign-repair",
	Repository:       "/repo",
	BaseRevision:     "abc123",
	BuildTarget:      "./cmd/tool",
	OptimizationMode: domain.PolicyIdiomatic,
}

// recordingPolicy remembers the input of every decision it makes.
type recordingPolicy struct {
	sequencePolicy
	inputs []PolicyInput
}

func (p *recordingPolicy) Evaluate(ctx context.Context, input PolicyInput) (domain.Evaluation, error) {
	p.inputs = append(p.inputs, input)
	return p.sequencePolicy.Evaluate(ctx, input)
}

// TestRepairedRoleOutputIsRecorded: an answer the decoder had to salvage used
// to be indistinguishable from the one the model sent. A patch cut off at the
// output-token cap now reaches the candidate's evidence with the repair named,
// every repaired role is reported, and a clean answer reports nothing.
func TestRepairedRoleOutputIsRecorded(t *testing.T) {
	var calls int
	truncated := `{"hypothesis":"buffer output","patch":["--- a/m.go","+++ b/m.go","@@ -1,1 +1,1 @@","-a()","+b(`
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", "{\"objective\":\"line one\nline two\",\"next_experiment\":\"n\"}", &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}}, &calls),
		Analyst:     staticAgent(t, "analyst", `{"candidate_hypotheses":["reuse buffer"]`, &calls),
		Optimizer:   staticAgent(t, "optimizer", truncated, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: true}, &calls),
	}
	policy := &recordingPolicy{sequencePolicy: sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}}}
	jobs := &fakeJobService{}
	orch := mustNew(t, Dependencies{Runner: &fakeRunnerService{}, Policy: policy, Jobs: jobs, Agents: roleSet},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-repair", repairCampaign, "finalize_campaign")

	want := []repairedRole{
		{role: "coordinator", repair: agents.RepairEscapedControlChars},
		{role: "analyst", repair: agents.RepairAddedClosers},
		{role: "optimizer", repair: agents.RepairTerminatedString},
	}
	if !slices.Equal(jobs.repaired, want) {
		t.Errorf("repaired = %+v, want %+v", jobs.repaired, want)
	}
	if len(policy.inputs) != 1 {
		t.Fatalf("policy calls = %d, want 1", len(policy.inputs))
	}
	evidence := policy.inputs[0].Evidence
	if evidence.ProposalRepair != agents.RepairTerminatedString {
		t.Errorf("proposal repair = %q, want %q", evidence.ProposalRepair, agents.RepairTerminatedString)
	}
	if evidence.Candidate.Hypothesis != "buffer output" {
		t.Errorf("hypothesis = %q: the salvaged proposal must still be the one evaluated", evidence.Candidate.Hypothesis)
	}
}

func TestSalvageAnalystResultReportsTheRepairOfWhatItReturns(t *testing.T) {
	result, repair, err := salvageAnalystResult(`{"hot_paths":[{"location":"a.go:1"}]`)
	if err != nil || len(result.HotPaths) != 1 || repair != agents.RepairAddedClosers {
		t.Errorf("salvageAnalystResult = %+v, %q, %v; want the repaired analysis", result, repair, err)
	}
	// The carried-in analysis wins over an empty answer and was never repaired.
	carried := map[string]any{"analysis": map[string]any{"hot_paths": []any{map[string]any{"location": "b.go:2"}}}}
	result, repair, err = salvageAnalystResult(carried)
	if err != nil || len(result.HotPaths) != 1 || repair != "" {
		t.Errorf("salvageAnalystResult(carried) = %+v, %q, %v; want the carried analysis, unrepaired", result, repair, err)
	}
}
