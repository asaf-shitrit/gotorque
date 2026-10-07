package orchestrator

import (
	"context"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

// Bench is the campaign's deterministic half as the graph sees it: everything
// that touches the repository, the toolchain, the measurements and the
// campaign's durable record, behind one interface. The engine is its only
// production adapter (internal/campaign); tests supply a fake. It is one port
// because the graph never reaches the deterministic half any other way, and
// the invariant that matters, that no model output produces a verdict, is a
// property of how the calls relate: Assess reaches the verdict before any
// advisory node has run, and Settle records it without recomputing it.
//
// The real seams stay where a campaign's behavior can vary: the optimizer
// (agents.Set, a live model or a stub) and the two Jev advisors (CauseAnalyst,
// ReviewAnalyst). A role failing is absorbed; the bench failing ends the run.
type Bench interface {
	// Discovery is the campaign's measured baseline evidence: the runs, the hot
	// functions and the profile the engine finished before the graph started.
	// The graph asks once per entry; nothing a cycle does changes it.
	Discovery(ctx context.Context) (DiscoveryEvidence, error)
	// Excerpts reads real source windows around the analysis's hot paths, so
	// optimizer patches carry valid context lines. The graph treats a failure
	// as the absence of excerpts, not as a reason to stop.
	Excerpts(ctx context.Context, analysis agents.AnalystResult) ([]SourceExcerpt, error)
	// Assess builds, tests and measures one proposal and judges the evidence
	// with the campaign's acceptance policy. The verdict is the deterministic
	// half's alone: it is reached here, before the reviewer runs, so no model
	// output can reach it.
	Assess(ctx context.Context, req CandidateRequest) (Assessment, error)
	// Settle makes a candidate's verdict durable. It records the verdict it is
	// given without computing or changing it, promotes the candidate when the
	// verdict accepted it, and persists the campaign's tallies, in an order
	// that cannot leave a record without its promotion. Everything a resumed
	// campaign derives its bounds from is on disk when Settle returns.
	Settle(ctx context.Context, s Settlement) error
	// Note records what the graph reports about the campaign's life. A note is
	// a record, never an input: the graph's decisions are made from its own
	// state. The graph ignores a failed degraded or repaired note (a store that
	// cannot take one is no reason to stop a campaign) and ends the run on a
	// failed started or finished one, as the store behind it is gone.
	Note(ctx context.Context, n Note) error
}
