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
}
