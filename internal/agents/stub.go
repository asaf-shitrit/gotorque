package agents

import (
	"encoding/json"
	"iter"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// NewDeterministicSet creates agents that emit valid typed JSON through ADK's
// normal agent nodes. It is intended for end-to-end harness smoke tests and
// never represents model judgment.
func NewDeterministicSet() (Set, error) {
	coordinator, err := stubAgent("coordinator", CoordinatorResult{Objective: "smoke test", NextExperiment: "inspect baseline"})
	if err != nil {
		return Set{}, err
	}
	explorer, err := stubAgent("explorer", ExplorerResult{EntryPoints: []string{"configured target"}, Proposals: []WorkloadProposal{{Name: "smoke workload", Tier: "plausible", Provenance: "stub", ExpectedValid: true}}})
	if err != nil {
		return Set{}, err
	}
	analyst, err := stubAgent("analyst", AnalystResult{CandidateHypotheses: []string{"smoke candidate"}})
	if err != nil {
		return Set{}, err
	}
	optimizer, err := stubAgent("optimizer", OptimizerResult{Hypothesis: "smoke candidate", Patch: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n"})
	if err != nil {
		return Set{}, err
	}
	reviewer, err := stubAgent("reviewer", ReviewerResult{Proceed: true, BehaviorArgument: "stub review"})
	if err != nil {
		return Set{}, err
	}
	return Set{Coordinator: coordinator, Explorer: explorer, Analyst: analyst, Optimizer: optimizer, Reviewer: reviewer}, nil
}

func stubAgent[T any](name string, value T) (adkagent.Agent, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var output any
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, err
	}
	return adkagent.New(adkagent.Config{Name: name, Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			event := session.NewEvent(ctx, ctx.InvocationID())
			event.Output = output
			yield(event, nil)
		}
	}})
}
