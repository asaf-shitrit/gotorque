package orchestrator

import (
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/jev"
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

// failures lists, per role, the cycles in which that role's call fails.
type failures map[agents.Role][]int

func scriptedRoleSet(t *testing.T, fail failures) agents.Set {
	t.Helper()
	return agents.Set{
		Coordinator: scriptedAgent(t, "coordinator", agents.CoordinatorResult{Objective: "cut runtime", NextExperiment: "patch parser"}, fail[agents.RoleCoordinator]...),
		Explorer:    scriptedAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"scan"}}, fail[agents.RoleExplorer]...),
		Analyst:     scriptedAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"reuse buffer"}}, fail[agents.RoleAnalyst]...),
		Optimizer:   scriptedAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "reuse buffer", Patch: "diff --git a/a.go b/a.go"}, fail[agents.RoleOptimizer]...),
		Reviewer:    scriptedAgent(t, "reviewer", agents.ReviewerResult{Proceed: true}, fail[agents.RoleReviewer]...),
	}
}

func everyRole(cycles ...int) failures {
	return failures{
		agents.RoleCoordinator: cycles,
		agents.RoleExplorer:    cycles,
		agents.RoleAnalyst:     cycles,
		agents.RoleOptimizer:   cycles,
		agents.RoleReviewer:    cycles,
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
// down every role degraded on every cycle, the optimizer's empty patch was
// rejected as a bad patch, and the campaign ran to its rejection bound and
// ended with a stop reason that blamed the patches. A cycle in which every
// model role failed now ends the campaign at once and names the provider,
// whichever bound the same cycle also met.
func TestProviderOutageStopsTheCampaign(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		maxCandidates, maxConsecutive int
	}{
		{name: "slack bounds", maxCandidates: 8, maxConsecutive: 4},
		{name: "the same cycle also spends the candidate budget", maxCandidates: 1, maxConsecutive: 4},
		{name: "the same cycle also meets the rejection bound", maxCandidates: 8, maxConsecutive: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &fakeJobService{}
			orch := mustNew(t, Dependencies{
				Runner: &fakeRunnerService{},
				Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
				Jobs:   jobs,
				Agents: scriptedRoleSet(t, everyRole(1, 2, 3, 4, 5, 6, 7, 8)),
			}, Config{MaxCandidates: tc.maxCandidates, MaxConsecutiveFailures: tc.maxConsecutive, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-outage", breakerCampaign, "finalize_campaign")
			assertProviderStop(t, result, jobs)
		})
	}
}

// assertProviderStop checks a campaign the breaker stopped after one cycle.
func assertProviderStop(t *testing.T, result CampaignResult, jobs *fakeJobService) {
	t.Helper()
	if result.CandidatesTried != 1 {
		t.Errorf("candidates tried = %d, want 1: one cycle of total failure is enough", result.CandidatesTried)
	}
	wantFailure := "reviewer: " + providerDown
	if result.ProviderFailure != wantFailure {
		t.Errorf("provider failure = %q, want %q", result.ProviderFailure, wantFailure)
	}
	if result.StopReason != stopReasonProviderFailure+wantFailure {
		t.Errorf("stop reason = %q, want the provider named", result.StopReason)
	}
	// The breaker ends the campaign; it does not touch the verdict. The empty
	// patch was still judged and recorded like any other.
	if len(jobs.progress) != 1 || jobs.progress[0].LastDecision != domain.DecisionRejected {
		t.Errorf("progress = %+v, want the one rejected verdict", jobs.progress)
	}
	if len(jobs.degraded) != 5 {
		t.Errorf("degraded = %+v, want all five roles recorded", jobs.degraded)
	}
}

// TestPartialRoleFailuresKeepTheCampaignRunning pins what the breaker must not
// catch: a role that fails and recovers is the transient case the degrading
// wrapper exists for, and failures do not add up across cycles.
func TestPartialRoleFailuresKeepTheCampaignRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail failures
	}{
		{name: "one transient failure", fail: failures{agents.RoleOptimizer: {1}}},
		{name: "every role but the reviewer, every cycle", fail: failures{
			agents.RoleCoordinator: {1, 2, 3}, agents.RoleExplorer: {1, 2, 3}, agents.RoleAnalyst: {1, 2, 3}, agents.RoleOptimizer: {1, 2, 3},
		}},
		// Every role fails at least once, but never all in the same cycle: a
		// tally that survived the cycle boundary would trip here.
		{name: "every role fails once, spread over two cycles", fail: failures{
			agents.RoleCoordinator: {1}, agents.RoleExplorer: {1},
			agents.RoleAnalyst: {2}, agents.RoleOptimizer: {2}, agents.RoleReviewer: {2},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch := mustNew(t, Dependencies{
				Runner: &fakeRunnerService{},
				Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
				Jobs:   &fakeJobService{},
				Agents: scriptedRoleSet(t, tc.fail),
			}, Config{MaxCandidates: 8, MaxConsecutiveFailures: 3, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
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

// TestProviderOutageDoesNotWaitForJevRoles: with --analyst jev, --reviewer
// jev and --explorer jev the analyst and reviewer are served by a different
// gateway on a different key, and the coordinator and explorer by code, so a
// model provider outage must trip the breaker even while all four keep
// answering. The analysis ranks targets, as Jev's always does, so the cycle
// runs the path a Jev campaign takes.
func TestProviderOutageDoesNotWaitForJevRoles(t *testing.T) {
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: targetLoop.Location}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	review := &fakeReviewAnalyst{result: agents.ReviewerResult{Proceed: true}}
	roles := scriptedRoleSet(t, everyRole(1, 2, 3, 4))
	planned, err := agents.PlannedExplorer()
	if err != nil {
		t.Fatal(err)
	}
	roles.Explorer, roles.ExploreEvaluator = planned, jev.Stub{}
	orch := mustNew(t, Dependencies{
		Runner: &hotRunner{},
		Policy: &sequencePolicy{decisions: []domain.Decision{domain.DecisionRejected}},
		Jobs:   &fakeJobService{},
		Agents: roles,
		Causes: analyst,
		Review: review,
	}, Config{MaxCandidates: 4, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-outage-jev", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 1 || !strings.HasPrefix(result.StopReason, stopReasonProviderFailure) {
		t.Errorf("tried %d, stop reason %q; want the breaker after one cycle", result.CandidatesTried, result.StopReason)
	}
	if len(analyst.requests) != 1 || len(review.requests) != 1 {
		t.Errorf("cause analyst calls = %d, review calls = %d, want 1 each", len(analyst.requests), len(review.requests))
	}
}

func TestModelRolesLeaveOutJevRoles(t *testing.T) {
	all := []string{"coordinator", "explorer", "analyst", "optimizer", "reviewer"}
	if got := (&campaignGraph{}).modelRoles(); !slices.Equal(got, all) {
		t.Errorf("model roles = %v, want %v", got, all)
	}
	withJev := []string{"explorer", "optimizer", "reviewer"}
	if got := (&campaignGraph{deps: Dependencies{Causes: &fakeCauseAnalyst{}}}).modelRoles(); !slices.Equal(got, withJev) {
		t.Errorf("model roles with a cause analyst = %v, want %v", got, withJev)
	}
	reviewed := []string{"coordinator", "explorer", "analyst", "optimizer"}
	if got := (&campaignGraph{deps: Dependencies{Review: &fakeReviewAnalyst{}}}).modelRoles(); !slices.Equal(got, reviewed) {
		t.Errorf("model roles with a review analyst = %v, want %v", got, reviewed)
	}
	both := []string{"explorer", "optimizer"}
	if got := (&campaignGraph{deps: Dependencies{Causes: &fakeCauseAnalyst{}, Review: &fakeReviewAnalyst{}}}).modelRoles(); !slices.Equal(got, both) {
		t.Errorf("model roles with both = %v, want %v", got, both)
	}
	everyJevRole := Dependencies{Causes: &fakeCauseAnalyst{}, Review: &fakeReviewAnalyst{}, Agents: agents.Set{ExploreEvaluator: jev.Stub{}}}
	if got := (&campaignGraph{deps: everyJevRole}).modelRoles(); !slices.Equal(got, []string{"optimizer"}) {
		t.Errorf("model roles with every Jev role = %v, want [optimizer]", got)
	}
}

func TestProviderFailureNeedsEveryModelRole(t *testing.T) {
	jev := &campaignGraph{deps: Dependencies{Causes: &fakeCauseAnalyst{}}}
	threeModelRoles := []RoleFailure{{"explorer", "b"}, {"optimizer", "c"}, {"reviewer", "d"}}
	if _, down := (&campaignGraph{}).providerFailure(CampaignState{CycleFailures: threeModelRoles}); down {
		t.Error("tripped while the coordinator and analyst, both model roles here, still answered")
	}
	failure, down := jev.providerFailure(CampaignState{CycleFailures: threeModelRoles})
	if !down || failure != "reviewer: d" {
		t.Errorf("providerFailure = %q, %v; want the last failure", failure, down)
	}
	if _, down := jev.providerFailure(CampaignState{}); down {
		t.Error("tripped on a cycle with no failures")
	}
}
