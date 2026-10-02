package orchestrator

import (
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
	"google.golang.org/adk/v2/session"
)

const providerDown = "POST /chat/completions: 401 Unauthorized"

// scriptedAgent answers with output except on the calls listed in failOn
// (1-based), where it fails the way a role does when its provider is down.
func scriptedAgent(t *testing.T, name string, output any, failOn ...int) adkagent.Agent {
	t.Helper()
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal %s output: %v", name, err)
	}
	var wire any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode %s output: %v", name, err)
	}
	calls := 0
	a, err := adkagent.New(adkagent.Config{
		Name: name,
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				calls++
				if slices.Contains(failOn, calls) {
					yield(nil, errors.New(providerDown))
					return
				}
				ev := session.NewEvent(ctx, ctx.InvocationID())
				ev.Output = wire
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("create %s agent: %v", name, err)
	}
	return a
}

// scriptedOptimizerSet is the one model role, failing on the listed cycles
// the way it does when its provider is down.
func scriptedOptimizerSet(t *testing.T, failOn ...int) agents.Set {
	t.Helper()
	return agents.Set{
		Optimizer: scriptedAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, failOn...),
	}
}

var breakerCampaign = CampaignRequest{
	CampaignID:       "campaign-breaker",
	Repository:       "/repo",
	BaseRevision:     "abc123",
	BuildTarget:      "./cmd/tool",
	OptimizationMode: domain.PolicyIdiomatic,
}

// TestProviderOutageStopsTheCampaign pins the circuit breaker. With the provider
// down the optimizer degraded on every cycle, its empty patch was rejected as a
// bad patch, and the campaign ran to its rejection bound and ended with a stop
// reason that blamed the patches. Two consecutive cycles in which the
// optimizer failed now end the campaign and name the provider, whichever bound
// the second cycle also met.
func TestProviderOutageStopsTheCampaign(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		maxCandidates, maxConsecutive int
	}{
		{name: "slack bounds", maxCandidates: 8, maxConsecutive: 4},
		{name: "the second cycle also spends the candidate budget", maxCandidates: 2, maxConsecutive: 4},
		{name: "the second cycle also meets the rejection bound", maxCandidates: 8, maxConsecutive: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &fakeJobService{}
			orch := mustNew(t, Dependencies{
				Runner: &fakeRunnerService{},
				Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
				Jobs:   jobs,
				Agents: scriptedOptimizerSet(t, 1, 2, 3, 4, 5, 6, 7, 8),
			}, Config{MaxCandidates: tc.maxCandidates, MaxConsecutiveFailures: tc.maxConsecutive, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-outage", breakerCampaign, "finalize_campaign")
			assertProviderStop(t, result, jobs)
		})
	}
}

// assertProviderStop checks a campaign the breaker stopped after two cycles.
func assertProviderStop(t *testing.T, result CampaignResult, jobs *fakeJobService) {
	t.Helper()
	if result.CandidatesTried != 2 {
		t.Errorf("candidates tried = %d, want 2: two cycles of optimizer failure are enough", result.CandidatesTried)
	}
	wantFailure := "optimizer: " + providerDown
	if result.ProviderFailure != wantFailure {
		t.Errorf("provider failure = %q, want %q", result.ProviderFailure, wantFailure)
	}
	if result.StopReason != stopReasonProviderFailure+wantFailure {
		t.Errorf("stop reason = %q, want the provider named", result.StopReason)
	}
	// The breaker ends the campaign; it does not touch the verdict. The empty
	// patches were still judged and recorded like any other.
	if len(jobs.progress) != 2 || jobs.progress[1].LastDecision != domain.DecisionRejected {
		t.Errorf("progress = %+v, want the two rejected verdicts", jobs.progress)
	}
	if len(jobs.degraded) != 2 {
		t.Errorf("degraded = %+v, want the optimizer recorded once per cycle", jobs.degraded)
	}
}

// TestPartialFailuresKeepTheCampaignRunning pins what the breaker must not
// catch: an optimizer that fails and recovers is the transient case the
// degrading wrapper exists for, failures do not add up across cycles that are
// not consecutive, and a failing Jev role is never a provider outage.
func TestPartialFailuresKeepTheCampaignRunning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failOn    []int
		jevFailed bool
	}{
		{name: "one transient failure", failOn: []int{1}},
		// A tally that survived a successful cycle would trip on the second
		// failure here.
		{name: "the optimizer fails every other cycle", failOn: []int{1, 3}},
		{name: "both Jev roles fail every cycle", jevFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := Dependencies{
				Runner: &fakeRunnerService{},
				Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
				Jobs:   &fakeJobService{},
				Agents: scriptedOptimizerSet(t, tc.failOn...),
			}
			if tc.jevFailed {
				deps.Causes = &fakeCauseAnalyst{err: errors.New(providerDown)}
				deps.Review = &fakeReviewAnalyst{err: errors.New(providerDown)}
			}
			orch := mustNew(t, deps, Config{MaxCandidates: 8, MaxConsecutiveFailures: 3, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-partial", breakerCampaign, "finalize_campaign")

			if result.CandidatesTried != 3 || result.StopReason != stopReasonConsecutiveFailures {
				t.Errorf("tried %d, stop reason %q; want 3 candidates and %q", result.CandidatesTried, result.StopReason, stopReasonConsecutiveFailures)
			}
			if result.ProviderFailure != "" {
				t.Errorf("provider failure = %q, want none", result.ProviderFailure)
			}
		})
	}
}

// TestProviderOutageTripsWhileJevKeepsAnswering: the analyst and reviewer are
// served by a different gateway on a different key, and the coordinator and
// explorer by code, so a model provider outage must trip the breaker even while
// all four keep answering. The optimizer is the only model role, so the
// breaker waits for a second failed cycle (outageCycles). The analysis ranks
// targets, as Jev's always does.
func TestProviderOutageTripsWhileJevKeepsAnswering(t *testing.T) {
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: targetLoop.Location}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	review := &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: true}}
	orch := mustNew(t, Dependencies{
		Runner: &hotRunner{},
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
		Jobs:   &fakeJobService{},
		Agents: scriptedOptimizerSet(t, 1, 2, 3, 4),
		Causes: analyst,
		Review: review,
	}, Config{MaxCandidates: 4, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-outage-jev", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 2 || !strings.HasPrefix(result.StopReason, stopReasonProviderFailure) {
		t.Errorf("tried %d, stop reason %q; want the breaker after two cycles", result.CandidatesTried, result.StopReason)
	}
	if len(analyst.requests) != 2 || len(review.requests) != 2 {
		t.Errorf("cause analyst calls = %d, review calls = %d, want 2 each", len(analyst.requests), len(review.requests))
	}
}

// TestProviderFailureCountsOnlyTheOptimizer: Jev roles fail on a different
// endpoint, so their failures never make a cycle an outage cycle, and the
// failure reported is the optimizer's even when a Jev role failed after it.
func TestProviderFailureCountsOnlyTheOptimizer(t *testing.T) {
	jevOnly := CampaignState{CycleFailures: []RoleFailure{{"analyst", "a"}, {"reviewer", "b"}}}
	if _, down := providerFailure(jevOnly); down {
		t.Error("tripped while the optimizer, the only model role, still answered")
	}
	withOptimizer := CampaignState{CycleFailures: []RoleFailure{{"analyst", "a"}, {"optimizer", "c"}, {"reviewer", "d"}}}
	failure, down := providerFailure(withOptimizer)
	if !down || failure != "optimizer: c" {
		t.Errorf("providerFailure = %q, %v; want the optimizer's failure", failure, down)
	}
	if _, down := providerFailure(CampaignState{}); down {
		t.Error("tripped on a cycle with no failures")
	}
}

// TestALoneModelRoleSurvivesOneFailedCycle: with Jev and code serving every
// role but the optimizer, one cycle in which the optimizer's call fails is the
// transient case, not an outage; the campaign runs on and spends its budget.
func TestALoneModelRoleSurvivesOneFailedCycle(t *testing.T) {
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: targetLoop.Location}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	review := &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: true}}
	orch := mustNew(t, Dependencies{
		Runner: &hotRunner{},
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
		Jobs:   &fakeJobService{},
		Agents: scriptedOptimizerSet(t, 2),
		Causes: analyst,
		Review: review,
	}, Config{MaxCandidates: 3, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-lone-role", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 3 || result.StopReason != stopReasonMaxCandidates || result.ProviderFailure != "" {
		t.Errorf("tried %d, stop reason %q, provider failure %q; want the whole budget spent", result.CandidatesTried, result.StopReason, result.ProviderFailure)
	}
}
