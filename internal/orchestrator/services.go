package orchestrator

import (
	"context"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
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

// JobService persists the asynchronous campaign lifecycle for a CLI control
// plane. The workflow itself remains independent of storage.
type JobService interface {
	StartCampaign(ctx context.Context, req CampaignRequest) (domain.Job, error)
	CompleteCampaign(ctx context.Context, job domain.Job, result CampaignResult) (domain.Job, error)
	// RecordRoleDegraded reports a role whose model call failed in a way the
	// graph absorbed: the node continues with an empty result rather than
	// ending the campaign. Without it the cause exists only on the process's
	// stderr, where no report and no API consumer can read it, and a candidate
	// that arrived empty is explained as "patch is empty".
	RecordRoleDegraded(ctx context.Context, role string, cause error) error
	// RecordRoleRepaired reports a role whose output parsed only after the
	// decoder rewrote it: control characters or quotes escaped, closers added,
	// or a string closed where the output was cut off. The repaired value is
	// used as the role's answer, so without the record a salvaged answer reads
	// exactly like an intended one. It is advisory and changes no decision.
	RecordRoleRepaired(ctx context.Context, role string, repair agents.Repair) error
}
