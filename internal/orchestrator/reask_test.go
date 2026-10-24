package orchestrator

import (
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// applyRefusal is a refusal shaped like the one candidate.Prepare reports when
// git refuses a hand-written diff.
const applyRefusal = "the patch does not apply to the base revision: git apply check: exit status 1: error: patch failed: statements.go:26"

// handWritten is an answer that ignores the target's function_source transport
// (ADR 0022) and hand-writes a diff, which is the shape issue #72 loses
// attempts to.
var handWritten = agents.OptimizerResult{Hypothesis: "hoist the lookup", Patch: "--- a/statements.go\n+++ b/statements.go\n@@ -26,2 +26,2 @@\n-\ta\n+\tb"}

// asFunctionSource is the same remedy sent over the deterministic transport.
var asFunctionSource = agents.OptimizerResult{Hypothesis: "hoist the lookup", FunctionSource: `func (statement) String() string { return "" }`}

// sequenceAgent answers with a different output on each call and records the
// prompt each call was handed, so a test can script both answers of one
// attempt and read what the graph told the optimizer.
func sequenceAgent[T any](t *testing.T, name string, outputs ...T) (adkagent.Agent, *[]string) {
	t.Helper()
	wires := make([]any, 0, len(outputs))
	for _, output := range outputs {
		encoded, err := json.Marshal(output)
		if err != nil {
			t.Fatalf("marshal %s output: %v", name, err)
		}
		var wire any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatalf("decode %s output: %v", name, err)
		}
		wires = append(wires, wire)
	}
	var prompts []string
	a, err := adkagent.New(adkagent.Config{
		Name: name,
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				var text strings.Builder
				if content := ctx.UserContent(); content != nil {
					for _, part := range content.Parts {
						text.WriteString(part.Text)
					}
				}
				prompts = append(prompts, text.String())
				ev := session.NewEvent(ctx, ctx.InvocationID())
				ev.Output = wires[min(len(prompts)-1, len(wires)-1)]
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("create %s agent: %v", name, err)
	}
	return a, &prompts
}

// reaskGraph is one campaign whose single cycle has a code-chosen target, so
// the graph may spend that attempt's one re-ask.
func reaskGraph(t *testing.T, bench *fakeBench, optimizer adkagent.Agent) *Orchestrator {
	t.Helper()
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{
		HotPaths: []agents.HotPath{{Location: targetLoop.Location}},
		Targets:  []agents.Target{targetLoop},
	}}
	return mustNew(t, Dependencies{Bench: bench, Agents: agents.Set{Optimizer: optimizer}, Causes: analyst},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
}

// TestApplyRefusalIsAskedAgainForFunctionSource pins issue #72's trigger: a
// code-chosen target, an answer that hand-wrote a patch and sent no
// function_source, and a patch git refused to apply. Nothing about the change
// was judged, so the attempt gets one more ask, over the transport the target
// asked for, with the refusal quoted. Both asks are the same attempt, and only
// the second answer is settled.
func TestApplyRefusalIsAskedAgainForFunctionSource(t *testing.T) {
	optimizer, prompts := sequenceAgent(t, "optimizer", handWritten, asFunctionSource)
	bench := &fakeBench{reaskCauses: []string{applyRefusal}, decisions: []domain.Decision{domain.DecisionRejected}}
	orch := reaskGraph(t, bench, optimizer)
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-reask", causeCampaign, "finalize_campaign")

	if len(*prompts) != 2 {
		t.Fatalf("optimizer calls = %d, want 2: one answer, then one re-ask", len(*prompts))
	}
	if !strings.Contains((*prompts)[1], "reask_cause") || !strings.Contains((*prompts)[1], applyRefusal) {
		t.Errorf("second ask did not quote the refusal: %s", (*prompts)[1])
	}
	if strings.Contains((*prompts)[0], "reask_cause") {
		t.Errorf("first ask carried a refusal it could not know yet: %s", (*prompts)[0])
	}
	assertReaskedOnce(t, bench)
}

// assertReaskedOnce holds the re-ask's bookkeeping: two assessments inside one
// attempt, the second answer settled and the first not, and one note naming the
// refusal.
func assertReaskedOnce(t *testing.T, bench *fakeBench) {
	t.Helper()
	proposals := bench.proposals()
	if len(proposals) != 2 {
		t.Fatalf("assessments = %d, want 2", len(proposals))
	}
	if proposals[0].Patch == "" || proposals[1].FunctionSource != asFunctionSource.FunctionSource || proposals[1].Patch != "" {
		t.Errorf("proposals = %+v, want the hand-written patch then the function_source", proposals)
	}
	if bench.requests[0].Attempt != bench.requests[1].Attempt {
		t.Errorf("attempts = %d and %d, want one attempt re-asked, not a second one",
			bench.requests[0].Attempt, bench.requests[1].Attempt)
	}
	assertSecondAnswerSettled(t, bench)
	if len(bench.reasked) != 1 || bench.reasked[0].role != string(agents.RoleOptimizer) || !strings.Contains(bench.reasked[0].cause, applyRefusal[:40]) {
		t.Errorf("re-ask notes = %+v, want one naming the optimizer and the refusal", bench.reasked)
	}
}

// assertSecondAnswerSettled holds that the answer the re-ask earned is the one
// the campaign judged.
func assertSecondAnswerSettled(t *testing.T, bench *fakeBench) {
	t.Helper()
	if len(bench.settled) != 1 || bench.settled[0].Assessment.Evidence.Candidate.ID != "candidate-2" {
		t.Fatalf("settled = %+v, want exactly the second answer's verdict", bench.settled)
	}
}

// TestApplyRefusalIsAskedAgainOnce: the re-ask is one ask, not a loop. An
// optimizer that hand-writes a patch again, and has it refused again, keeps the
// rejection, and the attempt ends where it always would have.
func TestApplyRefusalIsAskedAgainOnce(t *testing.T) {
	optimizer, prompts := sequenceAgent(t, "optimizer", handWritten)
	bench := &fakeBench{reaskCauses: []string{applyRefusal, applyRefusal}}
	orch := reaskGraph(t, bench, optimizer)
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-reask-once", causeCampaign, "finalize_campaign")

	if len(*prompts) != 2 {
		t.Errorf("optimizer calls = %d, want 2: one re-ask and no more", len(*prompts))
	}
	if len(bench.settled) != 1 || !bench.settled[0].Assessment.Evidence.Unmeasured {
		t.Fatalf("settled = %+v, want the second refusal's unmeasured verdict", bench.settled)
	}
	if len(bench.reasked) != 1 {
		t.Errorf("re-ask notes = %+v, want one", bench.reasked)
	}
}

// TestRefusedPatchWithNoTargetIsNotAskedAgain: without a code-chosen target
// there is no function_source transport to ask for, so the re-ask would be the
// same patch question twice. The rejection stands.
func TestRefusedPatchWithNoTargetIsNotAskedAgain(t *testing.T) {
	optimizer, prompts := sequenceAgent(t, "optimizer", handWritten)
	bench := &fakeBench{reaskCauses: []string{applyRefusal}}
	orch := mustNew(t, Dependencies{Bench: bench, Agents: agents.Set{Optimizer: optimizer}},
		Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	req := CampaignRequest{CampaignID: "c", Repository: "/repo", BaseRevision: "abc", BuildTarget: "./cmd/tool", OptimizationMode: domain.PolicyIdiomatic}
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-reask-notarget", req, "finalize_campaign")

	if len(*prompts) != 1 {
		t.Errorf("optimizer calls = %d, want 1", len(*prompts))
	}
	if len(bench.reasked) != 0 {
		t.Errorf("re-ask notes = %+v, want none", bench.reasked)
	}
}

// TestRefusedPatchWithAFunctionSourceIsNotAskedAgain: a proposal that already
// carries function_source keeps the precedence ADR 0022 gives its patch, so a
// refusal there is not the missing-transport failure the re-ask answers, and
// the attempt is not spent twice on it.
func TestRefusedPatchWithAFunctionSourceIsNotAskedAgain(t *testing.T) {
	both := agents.OptimizerResult{Hypothesis: "h", Patch: handWritten.Patch, FunctionSource: asFunctionSource.FunctionSource}
	optimizer, prompts := sequenceAgent(t, "optimizer", both)
	bench := &fakeBench{reaskCauses: []string{applyRefusal}}
	orch := reaskGraph(t, bench, optimizer)
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-reask-both", causeCampaign, "finalize_campaign")

	if len(*prompts) != 1 {
		t.Errorf("optimizer calls = %d, want 1", len(*prompts))
	}
	if len(bench.settled) != 1 {
		t.Errorf("settled = %+v, want the refusal recorded once", bench.settled)
	}
}

// TestReaskSpendsTheCandidatesBudgetOnce: the re-ask is part of one attempt, so
// the campaign's candidate tally counts the settled verdict once and the next
// cycle starts a fresh attempt.
func TestReaskSpendsTheCandidatesBudgetOnce(t *testing.T) {
	optimizer, prompts := sequenceAgent(t, "optimizer", handWritten, asFunctionSource)
	bench := &fakeBench{reaskCauses: []string{applyRefusal}, decisions: []domain.Decision{domain.DecisionRejected}}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{
		HotPaths: []agents.HotPath{{Location: targetLoop.Location}},
		Targets:  []agents.Target{targetLoop, targetAlloc},
	}}
	orch := mustNew(t, Dependencies{Bench: bench, Agents: agents.Set{Optimizer: optimizer}, Causes: analyst},
		Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-reask-budget", causeCampaign, "finalize_campaign")

	if len(*prompts) != 3 {
		t.Fatalf("optimizer calls = %d, want 3: one re-ask in the first cycle, one ask in the second", len(*prompts))
	}
	if result.CandidatesTried != 2 {
		t.Errorf("candidates tried = %d, want 2: the re-ask is the same attempt", result.CandidatesTried)
	}
	if attempts := []int{bench.requests[0].Attempt, bench.requests[1].Attempt, bench.requests[2].Attempt}; attempts[0] != 1 || attempts[1] != 1 || attempts[2] != 2 {
		t.Errorf("attempt numbers = %v, want [1 1 2]", attempts)
	}
}
