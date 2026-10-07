package orchestrator

import (
	"encoding/json"
	"iter"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

var (
	targetLoop  = agents.Target{Location: "main.go:206", Function: "gron", Cause: "unbuffered_io", Remedy: "Route the writes in gron through bufio.", Z: 3.3}
	targetAlloc = agents.Target{Location: "statements.go:26", Function: "(statement).String", Cause: "alloc", Remedy: "Remove the allocation in String.", Z: 1.5}
)

func excerptsFor(locations ...string) []SourceExcerpt {
	out := make([]SourceExcerpt, 0, len(locations))
	for _, l := range locations {
		out = append(out, SourceExcerpt{Path: l, HotPath: l, Content: "source of " + l})
	}
	return out
}

func TestPlanTargetPicksTheFirstUntriedTarget(t *testing.T) {
	state := CampaignState{
		Analysis:        agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
		SourceExcerpts:  excerptsFor("main.go:206", "statements.go:26", "token.go:141"),
		PriorCandidates: []PriorCandidate{{Attempt: 1, Target: &targetLoop}},
	}
	planTarget(&state)
	if state.Target == nil || !reflect.DeepEqual(*state.Target, targetAlloc) {
		t.Fatalf("target = %+v, want the untried alloc target", state.Target)
	}
	if len(state.SourceExcerpts) != 1 || state.SourceExcerpts[0].HotPath != "statements.go:26" {
		t.Errorf("excerpts = %+v, want only the target's source", state.SourceExcerpts)
	}
}

// TestPlanTargetHonoursTargetsTriedBeforeAResume: the graph's own prior
// candidates restart empty on resume, so the persisted targets carry over.
func TestPlanTargetHonoursTargetsTriedBeforeAResume(t *testing.T) {
	state := CampaignState{
		Request:  CampaignRequest{PriorTargets: []agents.Target{targetLoop, targetAlloc}},
		Analysis: agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
	}
	planTarget(&state)
	if state.Target != nil {
		t.Errorf("target = %+v, want none once every target was tried", state.Target)
	}
}

func TestPlanTargetKeepsAllExcerptsWhenTheTargetHasNone(t *testing.T) {
	state := CampaignState{
		Analysis:       agents.AnalystResult{Targets: []agents.Target{targetLoop}},
		SourceExcerpts: excerptsFor("token.go:141", "identifier.go:52"),
	}
	planTarget(&state)
	if state.Target == nil || len(state.SourceExcerpts) != 2 {
		t.Errorf("target %+v with %d excerpts, want the target and every excerpt kept", state.Target, len(state.SourceExcerpts))
	}
}

// TestPlanTargetLeavesAnUnrankedAnalysisAlone: an analysis that ranks nothing
// clears the target and leaves the excerpts untouched.
func TestPlanTargetLeavesAnUnrankedAnalysisAlone(t *testing.T) {
	state := CampaignState{SourceExcerpts: excerptsFor("a.go:1", "b.go:2"), Target: &targetLoop}
	planTarget(&state)
	if state.Target != nil || len(state.SourceExcerpts) != 2 {
		t.Errorf("state changed on the unranked path: target %+v, %d excerpts", state.Target, len(state.SourceExcerpts))
	}
}

// TestCodeChoosesEachCycleTarget drives the graph for three cycles: each
// cycle attacks the next untried
// target, and once the targets run out the optimizer is left to choose.
func TestCodeChoosesEachCycleTarget(t *testing.T) {
	var calls int
	roleSet := agents.Set{
		Optimizer: staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "buffer output", Patch: "diff"}, &calls),
	}
	bench := hotBench()
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	orch := mustNew(t, Dependencies{Runner: bench, Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 3, MaxConsecutiveFailures: 3, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-targets", causeCampaign, "finalize_campaign")

	if result.CandidatesTried != 3 {
		t.Fatalf("candidates tried = %d, want 3", result.CandidatesTried)
	}
	if targets := bench.targets(); len(targets) != 3 || targets[0] == nil || !reflect.DeepEqual(*targets[0], targetLoop) ||
		targets[1] == nil || !reflect.DeepEqual(*targets[1], targetAlloc) || targets[2] != nil {
		t.Errorf("targets per verdict = %v, want the loop, then the allocation, then none", bench.targets())
	}
}

// inputRecorder is an optimizer that remembers the text of every input it was
// handed and answers with a fixed proposal.
func inputRecorder(t *testing.T, inputs *[]string) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "optimizer",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				var text strings.Builder
				if content := ctx.UserContent(); content != nil {
					for _, part := range content.Parts {
						text.WriteString(part.Text)
					}
				}
				*inputs = append(*inputs, text.String())
				ev := session.NewEvent(ctx, ctx.InvocationID())
				ev.Output = map[string]any{"hypothesis": "buffer output", "patch": []string{"diff"}}
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("create optimizer: %v", err)
	}
	return a
}

// TestTheOptimizerReadsOnlyItsBrief: with a code-chosen target the optimizer
// sees the target, its excerpt, the policy and the earlier attempts, and none
// of the discovery evidence or analysis it is told not to act on. Once the
// targets run out it chooses for itself, so it gets the full state again.
func TestTheOptimizerReadsOnlyItsBrief(t *testing.T) {
	var inputs []string
	roleSet := agents.Set{Optimizer: inputRecorder(t, &inputs)}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop}}}
	orch := mustNew(t, Dependencies{Runner: hotBench(), Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-brief", causeCampaign, "finalize_campaign")

	if len(inputs) != 2 {
		t.Fatalf("optimizer called %d times, want 2", len(inputs))
	}
	var brief map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputs[0]), &brief); err != nil {
		t.Fatalf("first input is not JSON: %v\n%s", err, inputs[0])
	}
	want := []string{"optimization_mode", "target"}
	for _, key := range want {
		if _, ok := brief[key]; !ok {
			t.Errorf("brief lacks %q: %s", key, inputs[0])
		}
	}
	for _, key := range []string{"discovery", "analysis", "inspection", "request"} {
		if _, ok := brief[key]; ok {
			t.Errorf("brief carries %q, which the optimizer is not to act on", key)
		}
	}
	var second map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputs[1]), &second); err != nil {
		t.Fatalf("second input is not JSON: %v", err)
	}
	if _, ok := second["discovery"]; !ok {
		t.Errorf("with no target left the optimizer should see the full state, got keys %v", slices.Collect(maps.Keys(second)))
	}
	if _, ok := second["prior_candidates"]; !ok {
		t.Errorf("second input lacks the first attempt")
	}
}

// TestAnUnmeasuredTargetGetsOneMoreAttempt: a candidate rejected before
// measurement said nothing about its target, so the next cycle attacks the
// same target with the reason in prior_candidates; a second unmeasured
// attempt closes it.
func TestAnUnmeasuredTargetGetsOneMoreAttempt(t *testing.T) {
	state := CampaignState{
		Analysis:        agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
		PriorCandidates: []PriorCandidate{{Attempt: 1, Target: &targetLoop, Unmeasured: true, FailureDetail: "the patch uses bufio without importing it"}},
	}
	planTarget(&state)
	if state.Target == nil || !reflect.DeepEqual(*state.Target, targetLoop) {
		t.Fatalf("target = %+v, want the loop again after an unmeasured attempt", state.Target)
	}

	state.PriorCandidates = append(state.PriorCandidates, PriorCandidate{Attempt: 2, Target: &targetLoop, Unmeasured: true})
	planTarget(&state)
	if state.Target == nil || !reflect.DeepEqual(*state.Target, targetAlloc) {
		t.Fatalf("target = %+v, want the next target after two unmeasured attempts", state.Target)
	}

	measured := CampaignState{
		Analysis:        agents.AnalystResult{Targets: []agents.Target{targetLoop, targetAlloc}},
		PriorCandidates: []PriorCandidate{{Attempt: 1, Target: &targetLoop}},
	}
	planTarget(&measured)
	if measured.Target == nil || !reflect.DeepEqual(*measured.Target, targetAlloc) {
		t.Fatalf("target = %+v, want a measured target closed at once", measured.Target)
	}
}

// TestTheGraphRetriesATargetItNeverMeasured drives two cycles: the first
// candidate never reaches measurement, and the second is told to attack the
// same target, with the target on the evaluation request both times.
func TestTheGraphRetriesATargetItNeverMeasured(t *testing.T) {
	var inputs []string
	roleSet := agents.Set{Optimizer: inputRecorder(t, &inputs)}
	analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
	bench := hotBench()
	bench.unmeasuredFirst = true
	orch := mustNew(t, Dependencies{Runner: bench, Jobs: &fakeJobService{}, Agents: roleSet, Causes: analyst},
		Config{MaxCandidates: 2, MaxConsecutiveFailures: 2, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
	runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-retry", causeCampaign, "finalize_campaign")

	if !allTargets(bench.targets(), targetLoop, 2) {
		t.Fatalf("targets per verdict = %v, want the loop twice", bench.targets())
	}
	if !allTargets(bench.requestTargets(), targetLoop, 2) {
		t.Errorf("evaluation requests carried targets %v, want the loop twice", bench.requestTargets())
	}
	if len(inputs) != 2 || !strings.Contains(inputs[1], "without importing it") {
		t.Errorf("the retry's brief should carry the first attempt's reason; got %d inputs", len(inputs))
	}
}

func allTargets(got []*agents.Target, want agents.Target, n int) bool {
	return len(got) == n && !slices.ContainsFunc(got, func(t *agents.Target) bool { return t == nil || !reflect.DeepEqual(*t, want) })
}

// TestPlanTargetKeepsExcerptsForEveryFunctionInASet: a throwaway_result
// target (Functions set) keeps the excerpts for the callee and every caller
// in its set, from whichever files they live in, plus each such file's
// header, and drops excerpts belonging to neither.
func TestPlanTargetKeepsExcerptsForEveryFunctionInASet(t *testing.T) {
	multi := agents.Target{
		Location: "a.go:9", Function: "(*Store).Get", Cause: "throwaway_result",
		Kind: agents.TargetFunctionSet,
		Callers: []agents.FunctionRef{
			{Name: "(*Store).IsPositive", Location: "b.go:3"},
		},
	}
	state := CampaignState{
		Analysis: agents.AnalystResult{Targets: []agents.Target{multi}},
		SourceExcerpts: []SourceExcerpt{
			{Path: "a.go", StartLine: 1, HotPath: "a.go:1", Content: "package fixture"},
			{Path: "a.go", StartLine: 5, HotPath: "a.go:9"},
			{Path: "b.go", StartLine: 1, HotPath: "b.go:1", Content: "package fixture"},
			{Path: "b.go", StartLine: 1, HotPath: "b.go:3"},
			{Path: "c.go", StartLine: 1, HotPath: "c.go:1"},
		},
	}
	planTarget(&state)
	if state.Target == nil {
		t.Fatal("target = nil, want the multi-function target")
	}
	kept := make([]string, 0, len(state.SourceExcerpts))
	for _, e := range state.SourceExcerpts {
		kept = append(kept, e.HotPath)
	}
	for _, want := range []string{"a.go:9", "b.go:3"} {
		if !slices.Contains(kept, want) {
			t.Errorf("excerpts %v missing %q", kept, want)
		}
	}
	if slices.Contains(kept, "c.go:1") {
		t.Errorf("excerpts %v should not carry a file outside the set", kept)
	}
}

// TestPlanTargetKeepsTheFileHeader: the header of the target's file travels
// with the target's window, whichever hot path of that file it was collected
// for, and headers of other files do not.
func TestPlanTargetKeepsTheFileHeader(t *testing.T) {
	state := CampaignState{
		Analysis: agents.AnalystResult{Targets: []agents.Target{targetAlloc}},
		SourceExcerpts: []SourceExcerpt{
			{Path: "statements.go", StartLine: 1, HotPath: "statements.go:5", Content: "package main"},
			{Path: "statements.go", StartLine: 2, HotPath: "statements.go:5"},
			{Path: "main.go", StartLine: 1, HotPath: "main.go:206", Content: "package main"},
			{Path: "statements.go", StartLine: 10, HotPath: "statements.go:26"},
		},
	}
	planTarget(&state)
	if len(state.SourceExcerpts) != 2 || state.SourceExcerpts[0].StartLine != 1 || state.SourceExcerpts[0].Path != "statements.go" || state.SourceExcerpts[1].HotPath != "statements.go:26" {
		t.Errorf("excerpts = %+v, want the statements.go header, then the target's window", state.SourceExcerpts)
	}
}

func TestOptimizerBriefCarriesEarlierCandidates(t *testing.T) {
	earlier := []PriorCandidate{{Hypothesis: "concat instead of Sprintf", Decision: "inconclusive"}}
	state := CampaignState{Request: CampaignRequest{EarlierCandidates: earlier}, Target: &agents.Target{Function: "f"}}
	brief := optimizerBrief(state)
	if len(brief.EarlierCandidates) != 1 || brief.EarlierCandidates[0].Hypothesis != "concat instead of Sprintf" {
		t.Fatalf("earlier candidates = %+v", brief.EarlierCandidates)
	}
}

func TestOptimizerBriefCarriesGoVersion(t *testing.T) {
	state := CampaignState{Request: CampaignRequest{GoVersion: "1.20"}, Target: &agents.Target{Function: "f"}}
	if got := optimizerBrief(state).GoVersion; got != "1.20" {
		t.Fatalf("go version = %q", got)
	}
}

func TestRoleFailureFindsTheLatestForTheRole(t *testing.T) {
	failures := []RoleFailure{{Role: "optimizer", Cause: "first"}, {Role: "reviewer", Cause: "r"}, {Role: "optimizer", Cause: "second"}}
	if got := roleFailure(failures, "optimizer"); got != "second" {
		t.Fatalf("got %q", got)
	}
	if got := roleFailure(failures, "analyst"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestAResumeKeepsAnUnmeasuredTargetsRetry: a campaign interrupted after one
// unmeasured attempt on the loop must spend its next candidate on the loop
// again, exactly as TestTheGraphRetriesATargetItNeverMeasured's uninterrupted
// run does (ADR 0017, amended). A record written before Unmeasured was
// persisted reads as measured and closes its target, as it always did.
func TestAResumeKeepsAnUnmeasuredTargetsRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unmeasured bool
		want       agents.Target
	}{
		{name: "unmeasured record retries its target", unmeasured: true, want: targetLoop},
		{name: "measured or older record closes it", unmeasured: false, want: targetAlloc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inputs []string
			bench := hotBench()
			bench.unmeasuredFirst = true
			analyst := &fakeCauseAnalyst{result: agents.AnalystResult{HotPaths: []agents.HotPath{{Location: "main.go:206"}}, Targets: []agents.Target{targetLoop, targetAlloc}}}
			orch := mustNew(t, Dependencies{Runner: bench, Jobs: &fakeJobService{}, Agents: agents.Set{Optimizer: inputRecorder(t, &inputs)}, Causes: analyst},
				Config{MaxCandidates: 2, MaxConsecutiveFailures: 3, DeterministicTimeout: time.Second, AgentTimeout: time.Second, MaxConcurrency: 1})
			req := causeCampaign
			loop := targetLoop
			req.RecordedCandidates = []PriorCandidate{{Attempt: 1, Decision: string(domain.DecisionRejected), Target: &loop, Unmeasured: tc.unmeasured, FailureDetail: "did not build"}}
			result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-resume-retry", req, "finalize_campaign")

			if result.CandidatesTried != 2 || !allTargets(bench.targets(), tc.want, 1) {
				t.Fatalf("tried %d, targets per verdict = %v; want one new candidate on %s", result.CandidatesTried, bench.targets(), tc.want.Function)
			}
			if len(inputs) != 1 || !strings.Contains(inputs[0], "did not build") {
				t.Errorf("the resumed brief should carry the recorded attempt's failure; got %d inputs", len(inputs))
			}
		})
	}
}
