package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

type fakeReviewAnalyst struct {
	requests []ReviewRequest
	result   agents.ReviewerResult
	err      error
}

func (f *fakeReviewAnalyst) ReviewPatch(_ context.Context, req ReviewRequest) (agents.ReviewerResult, error) {
	f.requests = append(f.requests, req)
	return f.result, f.err
}

func reviewGraph(t *testing.T, review *fakeReviewAnalyst) (*Orchestrator, *fakeBench) {
	t.Helper()
	var calls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "--- a/main.go\n+++ b/main.go\n"}, &calls),
	}
	bench := hotBench()
	orch := mustNew(t, Dependencies{
		Runner: bench, Agents: roleSet,
		Causes: &fakeCauseAnalyst{result: agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}}},
		Review: review,
	}, Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	return orch, bench
}

// TestReviewAnalystServesTheReviewerNode: the review sees the patch and its
// target, and its concerns reach the next cycle through prior_candidates
// instead of being discarded.
func TestReviewAnalystServesTheReviewerNode(t *testing.T) {
	review := &fakeReviewAnalyst{result: agents.ReviewerResult{Concerns: []string{"an error from a call that can fail is discarded"}}}
	orch, _ := reviewGraph(t, review)
	prior := collectPriorCandidates(t, orch, causeCampaign)

	if len(review.requests) != 2 || review.requests[0].Proposal.Hypothesis != "buffer output" || review.requests[0].Target == nil || !reflect.DeepEqual(*review.requests[0].Target, targetLoop) {
		t.Fatalf("review requests = %+v, want the patch and its target each cycle", review.requests)
	}
	if len(prior) == 0 || !slices.Equal(prior[0].ReviewConcerns, []string{"an error from a call that can fail is discarded"}) {
		t.Errorf("prior candidates = %+v, want the review's concerns carried forward", prior)
	}
}

func TestFailingReviewAnalystDegradesLikeAnyRole(t *testing.T) {
	review := &fakeReviewAnalyst{err: errors.New("gateway returned HTTP 429 for Jev")}
	orch, bench := reviewGraph(t, review)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-review-degraded", causeCampaign, "finalize_campaign")
	if result.CandidatesTried != 2 {
		t.Errorf("candidates tried = %d, want the campaign to continue", result.CandidatesTried)
	}
	var degraded int
	for _, d := range bench.degraded {
		if d.role == "reviewer" {
			degraded++
		}
	}
	if degraded != 2 {
		t.Errorf("reviewer degradations recorded = %d, want one per cycle", degraded)
	}
}
