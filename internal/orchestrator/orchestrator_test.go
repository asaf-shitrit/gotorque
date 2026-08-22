package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"example.com/go-agent-optimizer/internal/agents"
	"example.com/go-agent-optimizer/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type fakeRunnerService struct {
	discoverCalls int
	evaluateCalls int
	promoted      []string
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
	policy := &sequencePolicy{decisions: []domain.Decision{
		domain.DecisionAccepted,
		domain.DecisionRejected,
		domain.DecisionInconclusive,
	}}
	jobs := &fakeJobService{}

	orch, err := New(Dependencies{
		Runner: runnerService,
		Policy: policy,
		Jobs:   jobs,
		Agents: roleSet,
	}, Config{
		MaxCandidates:          8,
		MaxConsecutiveFailures: 2,
		DeterministicTimeout:   time.Second,
		AgentTimeout:           time.Second,
		MaxConcurrency:         1,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	r, err := adkrunner.NewInMemory("optimizer-test", orch.Agent)
	if err != nil {
		t.Fatalf("NewInMemory() error = %v", err)
	}
	req := CampaignRequest{
		CampaignID:       "campaign-1",
		Repository:       "/repo",
		BaseRevision:     "abc123",
		BuildTarget:      "./cmd/tool",
		CommandArgs:      []string{"scan"},
		OptimizationMode: domain.PolicyIdiomatic,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: string(payload)}}}

	var result CampaignResult
	for event, runErr := range r.Run(context.Background(), "user-1", "session-1", msg, adkagent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("workflow run error = %v", runErr)
		}
		if event == nil || event.Output == nil || event.NodeInfo == nil || !strings.Contains(event.NodeInfo.Path, "finalize_campaign") {
			continue
		}
		encoded, err := json.Marshal(event.Output)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
	}

	if result.CampaignID != req.CampaignID {
		t.Fatalf("campaign ID = %q, want %q", result.CampaignID, req.CampaignID)
	}
	if result.CandidatesTried != 3 {
		t.Errorf("candidates tried = %d, want 3", result.CandidatesTried)
	}
	if result.StopReason != "consecutive rejection/inconclusive limit reached" {
		t.Errorf("stop reason = %q", result.StopReason)
	}
	if len(result.AcceptedCandidates) != 1 || result.AcceptedCandidates[0] != "candidate-1" {
		t.Errorf("accepted candidates = %v, want [candidate-1]", result.AcceptedCandidates)
	}
	if got := runnerService.promoted; len(got) != 1 || got[0] != "candidate-1" {
		t.Errorf("promoted = %v, want [candidate-1]", got)
	}
	for role, calls := range map[string]int{
		"coordinator": coordinatorCalls,
		"explorer":    explorerCalls,
		"analyst":     analystCalls,
		"optimizer":   optimizerCalls,
		"reviewer":    reviewerCalls,
	} {
		if calls != 3 {
			t.Errorf("%s calls = %d, want 3", role, calls)
		}
	}
	if len(jobs.progress) != 3 || jobs.complete != 1 {
		t.Errorf("job progress/complete = %d/%d, want 3/1", len(jobs.progress), jobs.complete)
	}
}

func TestNewRejectsMissingDeterministicDependency(t *testing.T) {
	_, err := New(Dependencies{}, DefaultConfig())
	if err == nil || !strings.Contains(err.Error(), "runner service is required") {
		t.Fatalf("New() error = %v, want missing runner", err)
	}
}
