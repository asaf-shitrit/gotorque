package orchestrator

import (
	"context"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

// RunnerService owns reproducible workload execution, isolated candidates,
// measurement and the acceptance policy's verdict over what it measured.
type RunnerService interface {
	Discover(ctx context.Context, req DiscoveryRequest) (DiscoveryEvidence, error)
	// Assess builds, tests and measures one proposal and judges the evidence
	// with the campaign's acceptance policy. The verdict is the deterministic
	// half's alone: it is computed here, before the reviewer runs, so no model
	// output can reach it.
	Assess(ctx context.Context, req CandidateRequest) (Assessment, error)
}

// ExcerptCollector is an optional RunnerService capability: attaching real
// source excerpts around analyst hot paths so optimizer patches carry valid
// context lines. Kept separate from RunnerService so existing fakes compile.
type ExcerptCollector interface {
	CollectExcerpts(ctx context.Context, analysis agents.AnalystResult) ([]SourceExcerpt, error)
}

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

// Settler makes a candidate's verdict durable. Settle records the verdict it
// is given without computing or changing it, promotes the candidate when the
// verdict accepted it, and persists the campaign's tallies, so that everything
// a resumed campaign derives its bounds from is on disk before it returns.
type Settler interface {
	Settle(ctx context.Context, s Settlement) error
}

// Notifier carries what the graph reports about the campaign's life: that it
// started, that a role degraded or needed its output repaired, and that it
// finished. A note is a record, never an input: the graph's decisions are made
// from its own state, and the notifier's answer changes none of them. The
// graph ignores a failed degraded or repaired note (a store that cannot take
// one is no reason to stop a campaign); a failed started or finished note ends
// the run, as the store behind it is gone.
type Notifier interface {
	Note(ctx context.Context, n Note) error
}
