package orchestrator

import (
	"context"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

// CauseAnalyst serves the analyst node: it classifies discovery's measured hot
// functions with Jev and ranks the answers in code. Its output is advice to the
// optimizer; it never reaches the policy decision.
type CauseAnalyst interface {
	AnalyzeCauses(ctx context.Context, req CauseRequest) (agents.AnalystResult, error)
}

// ReviewAnalyst serves the reviewer node with Jev behaviour-hazard checks. Its
// answer is advice: the policy never reads it.
type ReviewAnalyst interface {
	ReviewPatch(ctx context.Context, req ReviewRequest) (agents.ReviewerResult, error)
}
