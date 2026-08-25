// Package orchestrator wires deterministic optimization services and
// judgment-heavy ADK agents into a bounded workflow graph.
package orchestrator

import (
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
)

// CampaignRequest identifies one CLI command or subcommand to optimize.
type CampaignRequest struct {
	CampaignID       string                    `json:"campaign_id"`
	Repository       string                    `json:"repository"`
	BaseRevision     string                    `json:"base_revision"`
	BuildTarget      string                    `json:"build_target"`
	CommandArgs      []string                  `json:"command_args,omitempty"`
	OptimizationMode domain.OptimizationPolicy `json:"optimization_mode"`
}

// Inspection is deterministic repository and target inventory.
type Inspection struct {
	Packages   []string          `json:"packages,omitempty"`
	Commands   []string          `json:"commands,omitempty"`
	Tests      []string          `json:"tests,omitempty"`
	Benchmarks []string          `json:"benchmarks,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// DiscoveryRequest joins agent strategy with immutable campaign context for
// the deterministic runner.
type DiscoveryRequest struct {
	Campaign    CampaignRequest          `json:"campaign"`
	Attempt     int                      `json:"attempt"`
	Coordinator agents.CoordinatorResult `json:"coordinator"`
	Explorer    agents.ExplorerResult    `json:"explorer"`
}

// DiscoveryEvidence is normalized measured evidence, with raw data referenced
// by artifact ID rather than embedded in model context.
type DiscoveryEvidence struct {
	RunIDs             []string          `json:"run_ids,omitempty"`
	ArtifactURIs       []string          `json:"artifact_uris,omitempty"`
	CoveredPaths       []string          `json:"covered_paths,omitempty"`
	HotFunctions       []string          `json:"hot_functions,omitempty"`
	ProfileSummaryPath string            `json:"profile_summary_path,omitempty"`
	Measurements       []domain.Metric   `json:"measurements,omitempty"`
	Summary            string            `json:"summary"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// CandidateRequest asks the deterministic runner to create an isolated
// candidate, build it, validate behavior, and collect comparable evidence.
type CandidateRequest struct {
	Campaign CampaignRequest        `json:"campaign"`
	Attempt  int                    `json:"attempt"`
	Analysis agents.AnalystResult   `json:"analysis"`
	Proposal agents.OptimizerResult `json:"proposal"`
}

// CandidateEvidence records the candidate and deterministic measurements used
// by the policy engine.
type CandidateEvidence struct {
	Candidate              domain.Candidate          `json:"candidate"`
	BehaviorMatches        bool                      `json:"behavior_matches"`
	SafetyChecksPassed     bool                      `json:"safety_checks_passed"`
	RepresentativeEvidence bool                      `json:"representative_evidence"`
	Comparisons            []domain.MetricComparison `json:"comparisons,omitempty"`
	RepSamples             []domain.WorkloadSamples  `json:"rep_samples,omitempty"`
	ValidationJobs         []string                  `json:"validation_jobs,omitempty"`
	ArtifactURIs           []string                  `json:"artifact_uris,omitempty"`
	BenchstatOutput        string                    `json:"benchstat_output,omitempty"`
	Summary                string                    `json:"summary"`
	// FailureDetail carries the structured failure output (patch or compiler
	// stderr tail) behind a rejection so later cycles can avoid repeating
	// the same failed approach.
	FailureDetail string `json:"failure_detail,omitempty"`
	// PgoComparisons and PgoNote record the informational PGO lane: one extra
	// interleaved A/B series in which baseline and candidate were both built
	// with the same discovery-derived pprof CPU profile. The lane attributes
	// the compiler's profile-guided effect on top of an accepted source patch;
	// it never changes accept/reject decisions.
	PgoComparisons []domain.MetricComparison `json:"pgo_comparisons,omitempty"`
	PgoNote        string                    `json:"pgo_note,omitempty"`
}

// PolicyInput contains all evidence needed for a deterministic decision.
type PolicyInput struct {
	Campaign CampaignRequest       `json:"campaign"`
	Evidence CandidateEvidence     `json:"evidence"`
	Review   agents.ReviewerResult `json:"review"`
}

// PriorCandidate records one already-evaluated proposal so later cycles
// propose different work instead of repeating rejected patches.
type PriorCandidate struct {
	Attempt       int      `json:"attempt"`
	Hypothesis    string   `json:"hypothesis"`
	Decision      string   `json:"decision"`
	Reasons       []string `json:"reasons,omitempty"`
	FailureDetail string   `json:"failure_detail,omitempty"`
}

// CampaignState is passed between graph nodes and mirrored into ADK session
// state. Deterministic nodes are the only writers of this structure.
type CampaignState struct {
	Request             CampaignRequest          `json:"request"`
	Job                 domain.Job               `json:"job"`
	Inspection          Inspection               `json:"inspection"`
	Coordinator         agents.CoordinatorResult `json:"coordinator"`
	Explorer            agents.ExplorerResult    `json:"explorer"`
	Discovery           DiscoveryEvidence        `json:"discovery"`
	Analysis            agents.AnalystResult     `json:"analysis"`
	Proposal            agents.OptimizerResult   `json:"proposal"`
	Candidate           CandidateEvidence        `json:"candidate"`
	Review              agents.ReviewerResult    `json:"review"`
	Evaluation          domain.Evaluation        `json:"evaluation"`
	PriorCandidates     []PriorCandidate         `json:"prior_candidates,omitempty"`
	CandidatesTried     int                      `json:"candidates_tried"`
	ConsecutiveFailures int                      `json:"consecutive_failures"`
	AcceptedCandidates  []string                 `json:"accepted_candidates,omitempty"`
	StartedAt           time.Time                `json:"started_at"`
	StopReason          string                   `json:"stop_reason,omitempty"`

	// SourceExcerpts is best-effort enrichment: real code around hot paths
	// so the optimizer can write patch context lines that git apply accepts.
	SourceExcerpts []SourceExcerpt `json:"source_excerpts,omitempty"`
}

// SourceExcerpt carries a slice of real repository source near a hot path.
type SourceExcerpt struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Content   string `json:"content"`
	HotPath   string `json:"hot_path"`
}

// CampaignProgress is persisted after each deterministic policy decision.
type CampaignProgress struct {
	CandidatesTried     int             `json:"candidates_tried"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	LastDecision        domain.Decision `json:"last_decision"`
	CandidateID         string          `json:"candidate_id"`
}

// CampaignResult is the graph's single terminal output.
type CampaignResult struct {
	CampaignID         string            `json:"campaign_id"`
	Job                domain.Job        `json:"job"`
	CandidatesTried    int               `json:"candidates_tried"`
	AcceptedCandidates []string          `json:"accepted_candidates,omitempty"`
	FinalEvaluation    domain.Evaluation `json:"final_evaluation"`
	StopReason         string            `json:"stop_reason"`
}
