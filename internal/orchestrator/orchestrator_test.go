package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

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

// TestCampaignSurvivesRoleFailures pins the campaign-level bound: a role whose
// model call fails costs one candidate, not the run. Without the degrading
// node the same failure ended the workflow, so a campaign that had built a
// baseline and run discovery was recorded as failed with nothing evaluated.
func TestCampaignSurvivesRoleFailures(t *testing.T) {
	// The optimizer fails on the first cycle only: two failed cycles in a row
	// would trip the provider breaker instead of the rejection bound.
	roleSet := scriptedOptimizerSet(t, 1)
	causes := &fakeCauseAnalyst{err: errors.New("provider stalled past the retry ladder")}
	runnerService := &fakeBench{decisions: []domain.Decision{domain.DecisionRejected, domain.DecisionRejected}}
	orch := mustNew(t, Dependencies{Runner: runnerService, Agents: roleSet, Causes: causes}, Config{
		MaxCandidates:          8,
		MaxConsecutiveFailures: 2,
		DeterministicTimeout:   time.Second,
		AgentTimeout:           time.Second,
		MaxConcurrency:         1,
	})
	req := CampaignRequest{
		CampaignID:       "campaign-degraded",
		Repository:       "/repo",
		BaseRevision:     "abc123",
		BuildTarget:      "./cmd/tool",
		CommandArgs:      []string{"scan"},
		OptimizationMode: domain.PolicyIdiomatic,
	}
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-1", req, "finalize_campaign")

	if result.CandidatesTried != 2 {
		t.Errorf("candidates tried = %d, want 2: the campaign must keep running on a failed role", result.CandidatesTried)
	}
	if result.StopReason != "consecutive rejection/inconclusive limit reached" {
		t.Errorf("stop reason = %q", result.StopReason)
	}
	if len(causes.requests) == 0 {
		t.Errorf("the failing cause analyst was never called")
	}
	// A degraded role used to report its cause to the process's stderr, where no
	// report could read it; the campaign now records it.
	seen := map[string]int{}
	for _, degraded := range runnerService.degraded {
		seen[degraded.role]++
		if !strings.Contains(degraded.cause, "provider stalled past the retry ladder") && !strings.Contains(degraded.cause, providerDown) {
			t.Errorf("degraded %s cause = %q, want the role's error", degraded.role, degraded.cause)
		}
	}
	if seen["analyst"] == 0 || seen["optimizer"] == 0 {
		t.Errorf("degraded roles recorded = %v, want both analyst and optimizer", seen)
	}
}

// withJev supplies the Jev-backed services the graph requires, for tests that
// do not care what they answer. A test that does passes its own. A runner that
// also settles and takes notes (the fakeBench) does both for the graph unless
// told otherwise.
func withJev(deps Dependencies) Dependencies {
	if settler, ok := deps.Runner.(Settler); ok && deps.Settler == nil {
		deps.Settler = settler
	}
	if notes, ok := deps.Runner.(Notifier); ok && deps.Notes == nil {
		deps.Notes = notes
	}
	if deps.Causes == nil {
		deps.Causes = &fakeCauseAnalyst{result: agents.AnalystResult{CandidateHypotheses: []string{"reuse buffer"}}}
	}
	if deps.Review == nil {
		deps.Review = &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: true}}
	}
	return deps
}

func mustNew(t *testing.T, deps Dependencies, cfg Config) *Orchestrator {
	t.Helper()
	orch, err := New(withJev(deps), cfg)
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
	var optimizerCalls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, &optimizerCalls),
	}
	causes := &fakeCauseAnalyst{result: agents.AnalystResult{CandidateHypotheses: []string{"reuse buffer"}}}
	review := &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: true, BehaviorArgument: "outputs unchanged"}}
	runnerService := &fakeBench{decisions: []domain.Decision{
		domain.DecisionAccepted,
		domain.DecisionRejected,
		domain.DecisionInconclusive,
	}}

	orch := mustNew(t, Dependencies{
		Runner: runnerService,
		Agents: roleSet,
		Causes: causes,
		Review: review,
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
		"analyst":   len(causes.requests),
		"optimizer": optimizerCalls,
		"reviewer":  len(review.requests),
	})
	if len(runnerService.settled) != 3 || runnerService.started != 1 || runnerService.finished != 1 {
		t.Errorf("settled/started/finished = %d/%d/%d, want 3/1/1", len(runnerService.settled), runnerService.started, runnerService.finished)
	}
	assertDiscoveredOnce(t, runnerService, causes)
}

// assertDiscoveredOnce: discovery's evidence belongs to the campaign, not to a
// cycle, so every cycle's analyst reads the one fetch rather than asking again.
func assertDiscoveredOnce(t *testing.T, bench *fakeBench, causes *fakeCauseAnalyst) {
	t.Helper()
	if bench.discoverCalls != 1 {
		t.Errorf("discovery ran %d times, want once", bench.discoverCalls)
	}
	for i, request := range causes.requests {
		if len(request.Discovery.RunIDs) != 1 || request.Discovery.RunIDs[0] != "run-1" {
			t.Errorf("cycle %d analyst saw discovery %v, want the one fetch run-1", i+1, request.Discovery.RunIDs)
		}
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
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, &calls),
	}
	runnerService := &fakeBench{failureDetail: ".go:9:2: undefined: fasterParse"}
	orch := mustNew(t, Dependencies{
		Runner: runnerService,
		Agents: roleSet,
		Review: &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: false, BehaviorArgument: "suspect"}},
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

// TestNewRequiresTheOptimizerAndBothJevServices: the analyst and reviewer are
// always Jev and the optimizer is the one model role, so a graph missing any
// of them has a node with nothing to run and must be refused at construction.
func TestNewRequiresTheOptimizerAndBothJevServices(t *testing.T) {
	var calls int
	bench := &fakeBench{}
	full := Dependencies{
		Runner:  bench,
		Settler: bench,
		Notes:   bench,
		Agents:  agents.Set{Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{}, &calls)},
		Causes:  &fakeCauseAnalyst{},
		Review:  &fakeReviewAnalyst{},
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Dependencies)
		wantErr string
	}{
		{name: "optimizer", mutate: func(d *Dependencies) { d.Agents = agents.Set{} }, wantErr: "optimizer agent is required"},
		{name: "cause analyst", mutate: func(d *Dependencies) { d.Causes = nil }, wantErr: "cause analyst is required"},
		{name: "review analyst", mutate: func(d *Dependencies) { d.Review = nil }, wantErr: "review analyst is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := full
			tc.mutate(&deps)
			_, err := New(deps, DefaultConfig())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if _, err := New(full, DefaultConfig()); err != nil {
		t.Fatalf("New() with every dependency error = %v", err)
	}
}

func TestCampaignGraphAcceptsStatisticallySupportedCandidate(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "preallocate slice", Patch: "diff --git a/a.go b/a.go"}, &calls),
	}
	// The verdict is the bench's: the policy that reaches it lives behind
	// Assess, and the graph's part is to carry an accepting verdict to Settle.
	runnerService := &fakeBench{decisions: []domain.Decision{domain.DecisionAccepted}}

	orch := mustNew(t, Dependencies{
		Runner: runnerService,
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
	if progress := runnerService.progress(); len(progress) == 0 || progress[0].LastDecision != domain.DecisionAccepted {
		t.Fatalf("progress decisions = %+v", progress)
	}
	if result.StopReason != "maximum candidate count reached" {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
}

// runExpectingError drives the graph to completion and returns the first run
// error, for request content the deterministic guards must refuse.
func runExpectingError(t *testing.T, orch *Orchestrator, sessionID string, req CampaignRequest) error {
	t.Helper()
	r := mustRunner(t, "optimizer-test", orch)
	for _, runErr := range r.Run(context.Background(), "user-1", sessionID, mustMessage(t, req), adkagent.RunConfig{}) {
		if runErr != nil {
			return runErr
		}
	}
	return nil
}

func rejectingRoleSet(t *testing.T, calls *int) agents.Set {
	t.Helper()
	return agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, calls),
	}
}

// TestConsecutiveFailureBoundCountsCarriedInFailures pins the bound to the
// campaign rather than to one process. A campaign interrupted by the OS and
// resumed re-enters the graph with a fresh CampaignState, so the tally has to
// be derived from the recorded candidates on the request; without it every
// resume bought a full new run of MaxConsecutiveFailures rejections.
func TestConsecutiveFailureBoundCountsCarriedInFailures(t *testing.T) {
	tests := []struct {
		name                   string
		priorFailures          int
		maxConsecutiveFailures int
		wantCandidates         int
	}{
		{name: "fresh campaign spends the whole allowance", priorFailures: 0, maxConsecutiveFailures: 4, wantCandidates: 4},
		{name: "resume spends only what the campaign has left", priorFailures: 3, maxConsecutiveFailures: 4, wantCandidates: 1},
		{name: "resume already at the bound evaluates nothing", priorFailures: 4, maxConsecutiveFailures: 4, wantCandidates: 0},
		{name: "resume past the bound evaluates nothing", priorFailures: 9, maxConsecutiveFailures: 4, wantCandidates: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			runnerService := &fakeBench{}
			orch := mustNew(t, Dependencies{
				Runner: runnerService,
				Agents: rejectingRoleSet(t, &calls),
			}, Config{
				// MaxCandidates is deliberately slack so the failure bound is
				// the only thing that can stop the graph.
				MaxCandidates:          16,
				MaxConsecutiveFailures: tc.maxConsecutiveFailures,
				DeterministicTimeout:   time.Second,
				AgentTimeout:           time.Second,
				MaxConcurrency:         1,
			})
			req := CampaignRequest{
				CampaignID:         "campaign-resume",
				Repository:         "/repo",
				BaseRevision:       "abc123",
				BuildTarget:        "./cmd/tool",
				OptimizationMode:   domain.PolicyIdiomatic,
				RecordedCandidates: rejectedLedger(tc.priorFailures),
			}
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", tc.name, req, "finalize_campaign")

			if result.CandidatesTried != tc.priorFailures+tc.wantCandidates {
				t.Errorf("candidates tried = %d, want %d recorded + %d new", result.CandidatesTried, tc.priorFailures, tc.wantCandidates)
			}
			if runnerService.assessCalls != tc.wantCandidates {
				t.Errorf("candidate evaluations = %d, want %d", runnerService.assessCalls, tc.wantCandidates)
			}
			if result.StopReason != stopReasonConsecutiveFailures {
				t.Errorf("stop reason = %q, want %q", result.StopReason, stopReasonConsecutiveFailures)
			}
		})
	}
}

// rejectedLedger is n recorded rejections, attempts 1..n.
func rejectedLedger(n int) []PriorCandidate {
	out := make([]PriorCandidate, n)
	for i := range out {
		out[i] = PriorCandidate{Attempt: i + 1, Decision: string(domain.DecisionRejected)}
	}
	return out
}

func TestInitializeRejectsAnInvalidRecordedDecision(t *testing.T) {
	var calls int
	orch := mustNew(t, Dependencies{
		Runner: &fakeBench{},
		Agents: rejectingRoleSet(t, &calls),
	}, DefaultConfig())
	err := runExpectingError(t, orch, "session-negative", CampaignRequest{
		CampaignID:         "campaign-invalid",
		Repository:         "/repo",
		BaseRevision:       "abc123",
		BuildTarget:        "./cmd/tool",
		RecordedCandidates: []PriorCandidate{{Attempt: 1, Decision: "maybe"}},
	})
	if err == nil || !strings.Contains(err.Error(), `recorded candidate 1 has invalid decision "maybe"`) {
		t.Fatalf("run error = %v, want the invalid recorded decision rejected", err)
	}
}

// With a separate inconclusive bound, an unresolved verdict no longer extends
// the failure streak, and a rejection breaks the run of unresolved verdicts.
func TestCountDecisionTracksBothStreaks(t *testing.T) {
	state := CampaignState{}
	countDecision(&state, domain.DecisionRejected, true)
	countDecision(&state, domain.DecisionRejected, true)
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d, want 2", state.ConsecutiveFailures)
	}

	countDecision(&state, domain.DecisionInconclusive, true)
	if state.ConsecutiveInconclusive != 1 {
		t.Fatalf("inconclusive = %d, want 1", state.ConsecutiveInconclusive)
	}
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d, want 2: a separate bound must not extend the failure streak", state.ConsecutiveFailures)
	}

	countDecision(&state, domain.DecisionRejected, true)
	if state.ConsecutiveFailures != 3 || state.ConsecutiveInconclusive != 0 {
		t.Fatalf("failures/inconclusive = %d/%d, want 3/0: a rejection breaks the unresolved run", state.ConsecutiveFailures, state.ConsecutiveInconclusive)
	}

	countDecision(&state, domain.DecisionAccepted, true)
	if state.ConsecutiveFailures != 0 || state.ConsecutiveInconclusive != 0 {
		t.Fatalf("an accepted candidate must clear both streaks: %d/%d", state.ConsecutiveFailures, state.ConsecutiveInconclusive)
	}
}

// The default (no separate bound) keeps the historical behavior exactly: an
// inconclusive verdict counts as a failure.
func TestCountDecisionKeepsTheCombinedBoundByDefault(t *testing.T) {
	state := CampaignState{}
	countDecision(&state, domain.DecisionInconclusive, false)
	countDecision(&state, domain.DecisionInconclusive, false)
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d, want 2", state.ConsecutiveFailures)
	}
}

// The bound the evidence called for: the same stream of unresolved verdicts
// that ends a default campaign at stop_after_failures keeps going when the
// manifest configures stop_after_inconclusive, so the patch budget gets spent.
func TestInconclusiveBoundRunsSeparatelyFromFailures(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maxInconclusive int
		wantTried       int
		wantStopReason  string
	}{
		{name: "default combines unresolved verdicts with failures", maxInconclusive: 0, wantTried: 2, wantStopReason: "consecutive rejection/inconclusive limit reached"},
		{name: "separate bound keeps evaluating", maxInconclusive: 4, wantTried: 4, wantStopReason: "consecutive inconclusive limit reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var optimizerCalls int
			roleSet := agents.Set{
				Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, &optimizerCalls),
			}
			// Every verdict is unresolved: nothing was accepted and nothing was
			// definitively rejected either.
			orch := mustNew(t, Dependencies{
				Runner: &fakeBench{decisions: []domain.Decision{domain.DecisionInconclusive}},
				Agents: roleSet,
			}, Config{
				MaxCandidates:              8,
				MaxConsecutiveFailures:     2,
				MaxConsecutiveInconclusive: tc.maxInconclusive,
				DeterministicTimeout:       time.Second,
				AgentTimeout:               time.Second,
				MaxConcurrency:             1,
			})
			req := CampaignRequest{
				CampaignID:       "campaign-inconclusive",
				Repository:       "/repo",
				BaseRevision:     "abc123",
				BuildTarget:      "./cmd/tool",
				CommandArgs:      []string{"scan"},
				OptimizationMode: domain.PolicyIdiomatic,
			}
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-1", req, "finalize_campaign")

			if result.CandidatesTried != tc.wantTried {
				t.Fatalf("candidates tried = %d, want %d", result.CandidatesTried, tc.wantTried)
			}
			if result.StopReason != tc.wantStopReason {
				t.Fatalf("stop reason = %q, want %q", result.StopReason, tc.wantStopReason)
			}
		})
	}
}

// assessing overrides what the bench's Assess returns, for the tests of what
// the graph does with an assessment it did not expect.
type assessing struct {
	fakeBench
	alter func(*Assessment)
}

func (a *assessing) Assess(ctx context.Context, req CandidateRequest) (Assessment, error) {
	assessment, err := a.fakeBench.Assess(ctx, req)
	a.alter(&assessment)
	return assessment, err
}

// TestGraphRefusesAnAssessmentItCannotBind: the verdict must be one of the
// three decisions and must be about the candidate it travels with. Nothing is
// settled for a verdict the graph cannot attribute.
func TestGraphRefusesAnAssessmentItCannotBind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		alter   func(*Assessment)
		wantErr string
	}{
		{name: "another candidate's verdict", alter: func(a *Assessment) { a.Verdict.CandidateID = "candidate-other" }, wantErr: `verdict candidate ID "candidate-other" does not match "candidate-1"`},
		{name: "no decision", alter: func(a *Assessment) { a.Verdict.Decision = "" }, wantErr: `invalid decision ""`},
		{name: "no candidate", alter: func(a *Assessment) { a.Evidence.Candidate.ID = "" }, wantErr: "empty candidate ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			bench := &assessing{alter: tc.alter}
			orch := mustNew(t, Dependencies{Runner: bench, Agents: rejectingRoleSet(t, &calls)}, DefaultConfig())
			err := runExpectingError(t, orch, "session-bind", CampaignRequest{CampaignID: "campaign-bind", Repository: "/repo", BaseRevision: "abc123", BuildTarget: "./cmd/tool"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("run error = %v, want %q", err, tc.wantErr)
			}
			if len(bench.settled) != 0 {
				t.Errorf("settled %d candidates, want none", len(bench.settled))
			}
		})
	}
}

// TestTheReviewNeverReachesTheVerdict: the verdict is reached when the
// candidate is assessed, before the reviewer runs, so a review that objects to
// everything cannot turn an accepted candidate away, and an approving one
// cannot rescue a rejected one. The settlement carries the review only to be
// recorded beside the verdict.
func TestTheReviewNeverReachesTheVerdict(t *testing.T) {
	for _, decision := range []domain.Decision{domain.DecisionAccepted, domain.DecisionRejected} {
		t.Run(string(decision), func(t *testing.T) {
			var calls int
			bench := &fakeBench{decisions: []domain.Decision{decision}}
			objecting := &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: false, Concerns: []string{"discards an error"}}}
			orch := mustNew(t, Dependencies{Runner: bench, Agents: rejectingRoleSet(t, &calls), Review: objecting},
				Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
			runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-review-verdict", breakerCampaign, "finalize_campaign")

			if len(bench.settled) != 1 {
				t.Fatalf("settled %d, want 1", len(bench.settled))
			}
			settled := bench.settled[0]
			if settled.Assessment.Verdict.Decision != decision {
				t.Errorf("settled verdict = %q, want the assessed %q", settled.Assessment.Verdict.Decision, decision)
			}
			if !slices.Equal(settled.Review.Concerns, []string{"discards an error"}) {
				t.Errorf("settled review = %+v, want the concerns recorded beside the verdict", settled.Review)
			}
		})
	}
}
