package orchestrator

import (
	"context"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

// RunnerService owns reproducible repository inspection, workload execution,
// isolated candidates, measurement, and temporary baseline promotion.
type RunnerService interface {
	Inspect(ctx context.Context, req CampaignRequest) (Inspection, error)
	Discover(ctx context.Context, req DiscoveryRequest) (DiscoveryEvidence, error)
	EvaluateCandidate(ctx context.Context, req CandidateRequest) (CandidateEvidence, error)
	PromoteCandidate(ctx context.Context, candidate domain.Candidate) error
}

// ExcerptCollector is an optional RunnerService capability: attaching real
// source excerpts around analyst hot paths so optimizer patches carry valid
// context lines. Kept separate from RunnerService so existing fakes compile.
type ExcerptCollector interface {
	CollectExcerpts(ctx context.Context, analysis agents.AnalystResult) ([]SourceExcerpt, error)
}

// CauseAnalyst is an optional deterministic replacement for the analyst agent.
// When Dependencies.Causes is set the analyst node calls it instead of a model:
// it classifies discovery's measured hot functions and returns the same
// AnalystResult the agent would, so merge_analysis and every later node are
// unchanged. Its output is advice to the optimizer like the agent's; it never
// reaches the policy decision.
type CauseAnalyst interface {
	AnalyzeCauses(ctx context.Context, req CauseRequest) (agents.AnalystResult, error)
}

// PolicyService is deterministic. Implementations compute accepted, rejected,
// or inconclusive from measurements and behavior gates; an agent cannot
// override the result.
type PolicyService interface {
	Evaluate(ctx context.Context, input PolicyInput) (domain.Evaluation, error)
}

// JobService persists the asynchronous campaign lifecycle for a CLI control
// plane. The workflow itself remains independent of storage.
type JobService interface {
	StartCampaign(ctx context.Context, req CampaignRequest) (domain.Job, error)
	RecordProgress(ctx context.Context, job domain.Job, progress CampaignProgress) error
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
