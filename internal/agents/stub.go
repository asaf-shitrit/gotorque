package agents

import (
	"encoding/json"
	"iter"

	"github.com/asaf-shitrit/gotorque/internal/jev"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// NewDeterministicSet creates a stub optimizer that emits valid typed JSON
// through ADK's normal agent node, and the no-network Jev stub. It is intended
// for end-to-end harness smoke tests and never represents model judgment.
func NewDeterministicSet() (Set, error) {
	optimizer, err := stubAgent(string(RoleOptimizer), OptimizerResult{Hypothesis: "smoke candidate", Patch: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n"})
	if err != nil {
		return Set{}, err
	}
	return Set{Optimizer: optimizer, Jev: jev.Stub{}}, nil
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
