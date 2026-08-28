package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	policy "example.com/gotorque/internal/policy"

	"example.com/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type fakeRunnerService struct {
	discoverCalls int
	evaluateCalls int
	promoted      []string
	// failureDetail is returned as CandidateEvidence.FailureDetail so tests
	// can verify rejection detail propagates into prior_candidates.
	failureDetail string
}

func (f *fakeRunnerService) Inspect(context.Context, CampaignRequest) (Inspection, error) {
	return Inspection{Packages: []string{"example.com/cli"}, Commands: []string{"scan"}}, nil
}

func (f *fakeRunnerService) Discover(context.Context, DiscoveryRequest) (DiscoveryEvidence, error) {
	f.discoverCalls++
	return DiscoveryEvidence{
		RunIDs:  []string{fmt.Sprintf("run-%d", f.discoverCalls)},
		Summary: "measured parser hot path",
	}, nil
}

func (f *fakeRunnerService) EvaluateCandidate(_ context.Context, req CandidateRequest) (CandidateEvidence, error) {
	f.evaluateCalls++
	id := fmt.Sprintf("candidate-%d", f.evaluateCalls)
	return CandidateEvidence{
		Candidate: domain.Candidate{
			ID:           id,
			BaseRevision: req.Campaign.BaseRevision,
			Hypothesis:   req.Proposal.Hypothesis,
			PatchPath:    id + ".patch",
		},
		BehaviorMatches: true,
		Summary:         "candidate measured",
		FailureDetail:   f.failureDetail,
	}, nil
}

func (f *fakeRunnerService) PromoteCandidate(_ context.Context, candidate domain.Candidate) error {
	f.promoted = append(f.promoted, candidate.ID)
	return nil
}

type sequencePolicy struct {
	decisions []domain.Decision
	calls     int
}

func (p *sequencePolicy) Evaluate(_ context.Context, input PolicyInput) (domain.Evaluation, error) {
	decision := p.decisions[min(p.calls, len(p.decisions)-1)]
	p.calls++
	return domain.Evaluation{
		CandidateID:     input.Evidence.Candidate.ID,
		Decision:        decision,
		BehaviorMatches: input.Evidence.BehaviorMatches,
	}, nil
}

type fakeJobService struct {
	progress []CampaignProgress
	complete int
}

func (*fakeJobService) StartCampaign(_ context.Context, req CampaignRequest) (domain.Job, error) {
	return domain.Job{
		ID:        "job-" + req.CampaignID,
		Kind:      "optimization_campaign",
		Status:    domain.JobRunning,
		CreatedAt: time.Unix(1, 0),
		UpdatedAt: time.Unix(1, 0),
	}, nil
}

func (f *fakeJobService) RecordProgress(_ context.Context, _ domain.Job, progress CampaignProgress) error {
	f.progress = append(f.progress, progress)
	return nil
}

func (f *fakeJobService) CompleteCampaign(_ context.Context, job domain.Job, _ CampaignResult) (domain.Job, error) {
	f.complete++
	job.Status = domain.JobSucceeded
	return job, nil
}

func staticAgent[T any](t *testing.T, name string, output T, calls *int) adkagent.Agent {
	t.Helper()
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal %s output: %v", name, err)
	}
	var wireOutput any
	if err := json.Unmarshal(encoded, &wireOutput); err != nil {
		t.Fatalf("decode %s output: %v", name, err)
	}
	a, err := adkagent.New(adkagent.Config{
		Name: name,
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				*calls++
				ev := session.NewEvent(ctx, ctx.InvocationID())
				ev.Output = wireOutput
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("create %s agent: %v", name, err)
	}
	return a
}

func mustNew(t *testing.T, deps Dependencies, cfg Config) *Orchestrator {
	t.Helper()
	orch, err := New(deps, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return orch
}

func mustRunner(t *testing.T, name string, orch *Orchestrator) *adkrunner.Runner {
	t.Helper()
	r, err := adkrunner.NewInMemory(name, orch.Agent)
	if err != nil {
		t.Fatalf("NewInMemory() error = %v", err)
	}
	return r
}

func mustMessage(t *testing.T, req any) *genai.Content {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: string(payload)}}}
}

func eventHasNode(event *session.Event, nodeName string) bool {
	return event != nil && event.Output != nil && event.NodeInfo != nil && strings.Contains(event.NodeInfo.Path, nodeName)
}

func mustDecode[T any](t *testing.T, output any) T {
	t.Helper()
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out
}

func tryDecode[T any](output any) (T, bool) {
	var out T
	encoded, err := json.Marshal(output)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		return out, false
	}
	return out, true
}

func runUntilNode[T any](t *testing.T, orch *Orchestrator, runnerName, user, sessionID string, req any, nodeName string) T {
	t.Helper()
	r := mustRunner(t, runnerName, orch)
	msg := mustMessage(t, req)
	var out T
	for event, runErr := range r.Run(context.Background(), user, sessionID, msg, adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("workflow run error = %v", runErr)
		}
		if !eventHasNode(event, nodeName) {
			continue
		}
		out = mustDecode[T](t, event.Output)
	}
	return out
}

func assertAcceptedPromoted(t *testing.T, accepted, promoted []string) {
	t.Helper()
	want := []string{"candidate-1"}
	if !slices.Equal(accepted, want) {
		t.Errorf("accepted candidates = %v, want %v", accepted, want)
	}
	if !slices.Equal(promoted, want) {
		t.Errorf("promoted = %v, want %v", promoted, want)
	}
}

func assertRoleCalls(t *testing.T, want int, calls map[string]int) {
	t.Helper()
	for role, n := range calls {
		if n != want {
			t.Errorf("%s calls = %d, want %d", role, n, want)
		}
	}
}

func TestCampaignGraphLoopsWithinDeterministicBounds(t *testing.T) {
	var coordinatorCalls, explorerCalls, analystCalls, optimizerCalls, reviewerCalls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "find a measured hot path", NextExperiment: "profile scan"}, &coordinatorCalls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}, WorkloadStrategies: []string{"size sweep"}}, &explorerCalls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"reuse buffer"}}, &analystCalls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, &optimizerCalls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: true, BehaviorArgument: "outputs unchanged"}, &reviewerCalls),
	}
	runnerService := &fakeRunnerService{}
	seq := &sequencePolicy{decisions: []domain.Decision{
		domain.DecisionAccepted,
		domain.DecisionRejected,
		domain.DecisionInconclusive,
	}}
	jobs := &fakeJobService{}

	orch := mustNew(t, Dependencies{
		Runner: runnerService,
		Policy: seq,
		Jobs:   jobs,
		Agents: roleSet,
	}, Config{
		MaxCandidates:          8,
		MaxConsecutiveFailures: 2,
		DeterministicTimeout:   time.Second,
		AgentTimeout:           time.Second,
		MaxConcurrency:         1,
	})
	req := CampaignRequest{
		CampaignID:       "campaign-1",
		Repository:       "/repo",
		BaseRevision:     "abc123",
		BuildTarget:      "./cmd/tool",
		CommandArgs:      []string{"scan"},
		OptimizationMode: domain.PolicyIdiomatic,
	}
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-1", req, "finalize_campaign")

	if result.CampaignID != req.CampaignID {
		t.Fatalf("campaign ID = %q, want %q", result.CampaignID, req.CampaignID)
	}
	if result.CandidatesTried != 3 {
		t.Errorf("candidates tried = %d, want 3", result.CandidatesTried)
	}
	if result.StopReason != "consecutive rejection/inconclusive limit reached" {
		t.Errorf("stop reason = %q", result.StopReason)
	}
	assertAcceptedPromoted(t, result.AcceptedCandidates, runnerService.promoted)
	assertRoleCalls(t, 3, map[string]int{
		"coordinator": coordinatorCalls,
		"explorer":    explorerCalls,
		"analyst":     analystCalls,
		"optimizer":   optimizerCalls,
		"reviewer":    reviewerCalls,
	})
	if len(jobs.progress) != 3 || jobs.complete != 1 {
		t.Errorf("job progress/complete = %d/%d, want 3/1", len(jobs.progress), jobs.complete)
	}
}

func collectPriorCandidates(t *testing.T, orch *Orchestrator, req CampaignRequest) []PriorCandidate {
	t.Helper()
	r := mustRunner(t, "optimizer-test", orch)
	msg := mustMessage(t, req)
	var prior []PriorCandidate
	for event, runErr := range r.Run(context.Background(), "user-1", "session-fd", msg, adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("workflow run error = %v", runErr)
		}
		if !eventHasNode(event, "apply_policy") {
			continue
		}
		state, ok := tryDecode[CampaignState](event.Output)
		if !ok {
			continue
		}
		if len(state.PriorCandidates) > 0 {
			prior = state.PriorCandidates
		}
	}
	return prior
}

func TestApplyPolicyCarriesFailureDetailIntoPriorCandidates(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "cut allocations", NextExperiment: "patch parser"}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"reuse buffer"}}, &calls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: false, BehaviorArgument: "suspect"}, &calls),
	}
	runnerService := &fakeRunnerService{failureDetail: ".go:9:2: undefined: fasterParse"}
	orch := mustNew(t, Dependencies{
		Runner: runnerService,
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
		Jobs:   &fakeJobService{},
		Agents: roleSet,
	}, Config{
		MaxCandidates:          1,
		MaxConsecutiveFailures: 1,
		DeterministicTimeout:   time.Second,
		AgentTimeout:           time.Second,
		MaxConcurrency:         1,
	})
	prior := collectPriorCandidates(t, orch, CampaignRequest{CampaignID: "campaign-fd", Repository: "/repo", BaseRevision: "abc123", BuildTarget: "./cmd/tool"})
	if len(prior) != 1 {
		t.Fatalf("prior candidates = %d, want 1", len(prior))
	}
	if prior[0].FailureDetail != runnerService.failureDetail {
		t.Fatalf("prior candidate failure detail = %q, want %q", prior[0].FailureDetail, runnerService.failureDetail)
	}
	if prior[0].Decision != string(domain.DecisionRejected) {
		t.Fatalf("decision = %q, want %q", prior[0].Decision, domain.DecisionRejected)
	}
}

func TestNewRejectsMissingDeterministicDependency(t *testing.T) {
	_, err := New(Dependencies{}, DefaultConfig())
	if err == nil || !strings.Contains(err.Error(), "runner service is required") {
		t.Fatalf("New() error = %v, want missing runner", err)
	}
}

// policyFromDefaultConfig mirrors the production adapter: it runs the real
// deterministic policy over the evidence the runner service produced, so the
// accept path is exercised without any model or build involvement.
type defaultConfigPolicy struct{}

func (defaultConfigPolicy) Evaluate(_ context.Context, input PolicyInput) (domain.Evaluation, error) {
	comparisons := make([]policy.Comparison, 0, len(input.Evidence.Comparisons))
	for _, c := range input.Evidence.Comparisons {
		comparisons = append(comparisons, policy.Comparison{Name: c.Name, Unit: c.Unit, Baseline: c.Baseline, Candidate: c.Candidate, StatisticallySupported: c.StatisticallyFit})
	}
	result := policy.Evaluate(policy.DefaultConfig(), policy.Evidence{
		BehaviorMatches:        input.Evidence.BehaviorMatches,
		SafetyChecksPassed:     input.Evidence.SafetyChecksPassed,
		RepresentativeEvidence: input.Evidence.RepresentativeEvidence,
		Comparisons:            comparisons,
	})
	return domain.Evaluation{
		CandidateID:     input.Evidence.Candidate.ID,
		Decision:        result.Decision,
		BehaviorMatches: input.Evidence.BehaviorMatches,
		Comparisons:     input.Evidence.Comparisons,
		Reasons:         result.Reasons,
	}, nil
}

// acceptingRunnerService returns fully passing evidence whose primary metric
// improved by 5% with statistically supported samples and clean guardrails:
// exactly what policy.DefaultConfig requires for acceptance.
type acceptingRunnerService struct {
	fakeRunnerService
}

func (a *acceptingRunnerService) EvaluateCandidate(_ context.Context, req CandidateRequest) (CandidateEvidence, error) {
	ev, err := a.fakeRunnerService.EvaluateCandidate(context.Background(), req)
	if err != nil {
		return ev, err
	}
	ev.SafetyChecksPassed = true
	ev.RepresentativeEvidence = true
	ev.Comparisons = []domain.MetricComparison{
		{Name: "wall_time_ns", Unit: "ns", Baseline: 1000000, Candidate: 950000, DeltaPercent: -5, StatisticallyFit: true},
		{Name: "cpu_time_ns", Unit: "ns", Baseline: 800000, Candidate: 800000, DeltaPercent: 0, StatisticallyFit: true},
		{Name: "peak_memory_bytes", Unit: "bytes", Baseline: 1024, Candidate: 1024, DeltaPercent: 0, StatisticallyFit: true},
		{Name: "binary_size_bytes", Unit: "bytes", Baseline: 4096, Candidate: 4100, DeltaPercent: 0.09765625, StatisticallyFit: true},
	}
	return ev, nil
}

func TestCampaignGraphAcceptsStatisticallySupportedCandidate(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "cut runtime", NextExperiment: "patch allocation"}, &calls),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}}, &calls),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"preallocate"}}, &calls),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "preallocate slice", Patch: "diff --git a/a.go b/a.go"}, &calls),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: true}, &calls),
	}
	runnerService := &acceptingRunnerService{fakeRunnerService{failureDetail: ""}}
	jobs := &fakeJobService{}

	orch := mustNew(t, Dependencies{
		Runner: runnerService,
		Policy: defaultConfigPolicy{},
		Jobs:   jobs,
		Agents: roleSet,
	}, Config{
		MaxCandidates:          1,
		MaxConsecutiveFailures: 2,
		DeterministicTimeout:   time.Second,
		AgentTimeout:           time.Second,
		MaxConcurrency:         1,
	})
	req := CampaignRequest{
		CampaignID:       "campaign-accept",
		Repository:       "/repo",
		BaseRevision:     "abc123",
		BuildTarget:      "./cmd/tool",
		CommandArgs:      []string{"scan"},
		OptimizationMode: domain.PolicyIdiomatic,
	}
	result := runUntilNode[CampaignResult](t, orch, "gotorque-accept-test", "user-1", "session-1", req, "finalize_campaign")

	assertAcceptedPromoted(t, result.AcceptedCandidates, runnerService.promoted)
	if len(jobs.progress) == 0 || jobs.progress[0].LastDecision != domain.DecisionAccepted {
		t.Fatalf("progress decisions = %+v", jobs.progress)
	}
	if result.StopReason != "maximum candidate count reached" {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
}
