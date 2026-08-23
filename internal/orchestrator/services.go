package orchestrator

import (
	"context"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

// RunnerService owns reproducible repository inspection, workload execution,
// isolated candidates, measurement, and temporary baseline promotion.
type RunnerService interface {
	Inspect(context.Context, CampaignRequest) (Inspection, error)
	Discover(context.Context, DiscoveryRequest) (DiscoveryEvidence, error)
	EvaluateCandidate(context.Context, CandidateRequest) (CandidateEvidence, error)
	PromoteCandidate(context.Context, domain.Candidate) error
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
	Evaluate(context.Context, PolicyInput) (domain.Evaluation, error)
}

// JobService persists the asynchronous campaign lifecycle for an MCP or CLI
// control plane. The workflow itself remains independent of storage.
type JobService interface {
	StartCampaign(context.Context, CampaignRequest) (domain.Job, error)
	RecordProgress(context.Context, domain.Job, CampaignProgress) error
	CompleteCampaign(context.Context, domain.Job, CampaignResult) (domain.Job, error)
}
