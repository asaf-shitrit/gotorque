package orchestrator

import (
	"slices"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
)

var repairCampaign = CampaignRequest{
	CampaignID:       "campaign-repair",
	Repository:       "/repo",
	BaseRevision:     "abc123",
	BuildTarget:      "./cmd/tool",
	OptimizationMode: domain.PolicyIdiomatic,
}

// TestRepairedRoleOutputIsRecorded: an answer the decoder had to salvage used
// to be indistinguishable from the one the model sent. A patch cut off at the
// output-token cap now reaches the candidate's evidence with the repair named,
// the repair is reported against the optimizer, the one role that decodes
// model output.
func TestRepairedRoleOutputIsRecorded(t *testing.T) {
	var calls int
	truncated := `{"hypothesis":"buffer output","patch":["--- a/m.go","+++ b/m.go","@@ -1,1 +1,1 @@","-a()","+b(`
	roleSet := agents.Set{Optimizer: staticAgent(t, "optimizer", truncated, &calls)}
	bench := &fakeBench{}
	orch := mustNew(t, Dependencies{Bench: bench, Agents: roleSet},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-repair", repairCampaign, "finalize_campaign")

	want := []repairedRole{
		{role: "optimizer", repair: agents.RepairTerminatedString},
	}
	if !slices.Equal(bench.repaired, want) {
		t.Errorf("repaired = %+v, want %+v", bench.repaired, want)
	}
	if len(bench.settled) != 1 {
		t.Fatalf("settlements = %d, want 1", len(bench.settled))
	}
	evidence := bench.settled[0].Assessment.Evidence
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
