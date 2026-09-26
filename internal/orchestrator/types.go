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
	// PriorConsecutiveFailures carries the campaign-wide run of rejected or
	// inconclusive candidates that a caller already recorded for this
	// campaign. The graph builds a fresh CampaignState on every entry, so a
	// campaign resumed after an interruption would otherwise restart the
	// MaxConsecutiveFailures tally at zero and the bound would hold only
	// within a single process rather than over the whole campaign.
	PriorConsecutiveFailures int `json:"prior_consecutive_failures,omitempty"`
	// PriorTargets are the targets earlier candidates of this campaign tried,
	// carried in for the same reason: a resumed campaign must not attack them
	// again.
	PriorTargets []agents.Target `json:"prior_targets,omitempty"`
	// PriorConsecutiveInconclusive carries the campaign-wide run of
	// inconclusive verdicts, for the same reason: a campaign that configures
	// its own inconclusive bound must have that streak survive a resume too.
	PriorConsecutiveInconclusive int `json:"prior_consecutive_inconclusive,omitempty"`
	// Objective is the manifest's performance primary metric after the
	// campaign's trade-off was applied (see ADR 0020), such as
	// "peak_memory_bytes" under --tradeoff lean. The cause analyst reads it to
	// rank allocation causes ahead of others when the objective is memory; it
	// is empty only in tests that build a CampaignRequest by hand.
	Objective string `json:"objective,omitempty"`
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
	// HotFunctionWeights is discovery's per-function profile hotness, keyed by
	// pprof's package-qualified symbol name. It ranks a throwaway_result
	// target's consuming callers (ADR 0027's addendum): call-site count alone
	// is a weak key when almost every caller ties at one site, as most of
	// dasel's do. Absent when discovery carried no profile (a benchmark-only
	// or otherwise empty discovery), in which case the ranking that consumes
	// it falls back to call-site count exactly as before this field existed.
	HotFunctionWeights map[string]float64 `json:"hot_function_weights,omitempty"`
}

// CauseRequest carries what a CauseAnalyst needs: the repository to read source
// from and the hot functions discovery measured.
type CauseRequest struct {
	Campaign  CampaignRequest   `json:"campaign"`
	Discovery DiscoveryEvidence `json:"discovery"`
}

// ReviewRequest carries what a ReviewAnalyst needs: the repository at the base
// revision, the patch and its hypothesis, and the target it was told to attack.
type ReviewRequest struct {
	Campaign  CampaignRequest        `json:"campaign"`
	Target    *agents.Target         `json:"target,omitempty"`
	Proposal  agents.OptimizerResult `json:"proposal"`
	Candidate CandidateEvidence      `json:"candidate"`
}

// CandidateRequest asks the deterministic runner to create an isolated
// candidate, build it, validate behavior, and collect comparable evidence.
type CandidateRequest struct {
	Campaign CampaignRequest        `json:"campaign"`
	Attempt  int                    `json:"attempt"`
	Analysis agents.AnalystResult   `json:"analysis"`
	Proposal agents.OptimizerResult `json:"proposal"`
	// Target is the function and cause code chose for this patch, or nil
	// when the optimizer chose. The patch-shape check holds the patch to it.
	Target *agents.Target `json:"target,omitempty"`
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
	// Unmeasured is set when the candidate was rejected before measurement:
	// its patch did not apply, failed the shape check, or did not build.
	Unmeasured bool `json:"unmeasured,omitempty"`
	// PgoComparisons and PgoNote record the informational PGO lane: one extra
	// interleaved A/B series in which baseline and candidate were both built
	// with the same discovery-derived pprof CPU profile. The lane attributes
	// the compiler's profile-guided effect on top of an accepted source patch;
	// it never changes accept/reject decisions.
	PgoComparisons []domain.MetricComparison `json:"pgo_comparisons,omitempty"`
	PgoNote        string                    `json:"pgo_note,omitempty"`
	// ProposalRepair names the rewrite the optimizer's output needed before it
	// parsed, or is empty when it parsed as sent. A patch cut off at the
	// output-token cap is closed by the decoder and has its hunk counts
	// recomputed by normalization, after which nothing else distinguishes it
	// from a patch the model meant to send. It is a note for the record; policy
	// never reads it.
	ProposalRepair agents.Repair `json:"proposal_repair,omitempty"`
}

// PolicyInput contains all evidence needed for a deterministic decision.
type PolicyInput struct {
	Campaign CampaignRequest       `json:"campaign"`
	Evidence CandidateEvidence     `json:"evidence"`
	Review   agents.ReviewerResult `json:"review"`
	// Target is what code told the optimizer to attack, recorded with the
	// verdict; it never changes the verdict.
	Target *agents.Target `json:"target,omitempty"`
}

// PriorCandidate records one already-evaluated proposal so later cycles
// propose different work instead of repeating rejected patches.
type PriorCandidate struct {
	Attempt       int      `json:"attempt"`
	Hypothesis    string   `json:"hypothesis"`
	Decision      string   `json:"decision"`
	Reasons       []string `json:"reasons,omitempty"`
	FailureDetail string   `json:"failure_detail,omitempty"`
	// Target is the (function, cause) this candidate was told to attack, so
	// later cycles move on to the next one.
	Target *agents.Target `json:"target,omitempty"`
	// Unmeasured marks a candidate rejected before it was measured, so its
	// target has not been judged and may be attacked once more.
	Unmeasured bool `json:"unmeasured,omitempty"`
	// ReviewConcerns are the behaviour hazards the review raised, so the next
	// patch can avoid repeating them.
	ReviewConcerns []string `json:"review_concerns,omitempty"`
}

// RoleFailure is one role call the graph absorbed instead of failing on.
type RoleFailure struct {
	Role  string `json:"role"`
	Cause string `json:"cause"`
}

// CampaignState is passed between graph nodes and mirrored into ADK session
// state. Deterministic code is the only writer of this structure: the function
// nodes, and the degrading wrapper, which records a failed role call in
// CycleFailures without ever reading the model output it failed to get.
type CampaignState struct {
	Request     CampaignRequest          `json:"request"`
	Job         domain.Job               `json:"job"`
	Inspection  Inspection               `json:"inspection"`
	Coordinator agents.CoordinatorResult `json:"coordinator"`
	Explorer    agents.ExplorerResult    `json:"explorer"`
	Discovery   DiscoveryEvidence        `json:"discovery"`
	Analysis    agents.AnalystResult     `json:"analysis"`
	// Target is the function and cause code chose for this cycle's patch, or
	// nil when the analysis ranks no causes and the optimizer chooses.
	Target              *agents.Target         `json:"target,omitempty"`
	Proposal            agents.OptimizerResult `json:"proposal"`
	Candidate           CandidateEvidence      `json:"candidate"`
	Review              agents.ReviewerResult  `json:"review"`
	Evaluation          domain.Evaluation      `json:"evaluation"`
	PriorCandidates     []PriorCandidate       `json:"prior_candidates,omitempty"`
	CandidatesTried     int                    `json:"candidates_tried"`
	ConsecutiveFailures int                    `json:"consecutive_failures"`
	// ConsecutiveInconclusive counts the run of inconclusive verdicts. It is
	// only consulted when the campaign configures stop_after_inconclusive;
	// otherwise an inconclusive verdict extends ConsecutiveFailures, as it
	// always has.
	ConsecutiveInconclusive int       `json:"consecutive_inconclusive,omitempty"`
	AcceptedCandidates      []string  `json:"accepted_candidates,omitempty"`
	StartedAt               time.Time `json:"started_at"`
	StopReason              string    `json:"stop_reason,omitempty"`
	// CycleFailures lists the role calls that failed and were absorbed during
	// the current coordinator-to-reviewer cycle. The degrading wrapper appends
	// to it and the route node clears it before the next cycle, so the route
	// can tell one flaky call from a provider that answered nothing all cycle.
	// It never outlives a cycle: a resumed campaign starts a fresh one, so it
	// needs no persistence of its own.
	CycleFailures []RoleFailure `json:"cycle_failures,omitempty"`
	// ProviderFailure is set when the route stopped the campaign because every
	// model role failed in the same cycle. It holds the last failure, as
	// "role: cause".
	ProviderFailure string `json:"provider_failure,omitempty"`
	// OutageCycles counts consecutive cycles, the current one included, in
	// which every model role failed. See outageCycles. Like CycleFailures it
	// needs no persistence: a resumed campaign starts a fresh count.
	OutageCycles int `json:"outage_cycles,omitempty"`

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
	CandidatesTried         int             `json:"candidates_tried"`
	ConsecutiveFailures     int             `json:"consecutive_failures"`
	ConsecutiveInconclusive int             `json:"consecutive_inconclusive,omitempty"`
	LastDecision            domain.Decision `json:"last_decision"`
	CandidateID             string          `json:"candidate_id"`
}

// CampaignResult is the graph's single terminal output.
type CampaignResult struct {
	CampaignID         string            `json:"campaign_id"`
	Job                domain.Job        `json:"job"`
	CandidatesTried    int               `json:"candidates_tried"`
	AcceptedCandidates []string          `json:"accepted_candidates,omitempty"`
	FinalEvaluation    domain.Evaluation `json:"final_evaluation"`
	StopReason         string            `json:"stop_reason"`
	// ProviderFailure is non-empty when the campaign stopped because every
	// model role failed in one cycle; it holds the last failure. The caller
	// reports such a campaign as failed rather than completed: no bound was
	// reached, and the patches it rejected were empty for want of a provider.
	ProviderFailure string `json:"provider_failure,omitempty"`
}
