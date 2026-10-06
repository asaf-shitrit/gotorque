package orchestrator

import (
	"errors"
	"strings"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

const jevOutOfCredits = "jev classified none of 12 hot functions; first failure: OpenRouter returned HTTP 402 for Jev: Insufficient credits"

// TestEnded pins every way a campaign can end, at the checkpoint it is
// decided. A failure is set only when a role could not answer; every bound
// ends the campaign as completed.
func TestEnded(t *testing.T) {
	ranked := CampaignState{Analysis: agents.AnalystResult{Targets: []agents.Target{targetLoop}}}
	planned := ranked
	planned.Target = &targetLoop
	analystDown := CampaignState{CycleFailures: []RoleFailure{{Role: "analyst", Cause: jevOutOfCredits}}}
	reviewerDown := CampaignState{CycleFailures: []RoleFailure{{Role: "reviewer", Cause: "timeout"}}}
	optimizerDown := CampaignState{CandidatesTried: 2, OutageCycles: 2, CycleFailures: []RoleFailure{{Role: "optimizer", Cause: "HTTP 402"}}}
	optimizerOnce := optimizerDown
	optimizerOnce.OutageCycles = 1

	for _, tc := range []struct {
		name        string
		stopRanking bool
		state       CampaignState
		at          checkpoint
		wantDone    bool
		wantReason  string
		wantFailure string
	}{
		{name: "fresh campaign starts", at: atStart},
		{name: "resumed at the failure bound", at: atStart, state: CampaignState{ConsecutiveFailures: 3}, wantDone: true, wantReason: stopReasonConsecutiveFailures},
		{name: "planned target continues", stopRanking: true, at: afterAnalysis, state: planned},
		{name: "free choice continues without a target", at: afterAnalysis, state: analystDown},
		{name: "every flagged target tried", stopRanking: true, at: afterAnalysis, state: ranked, wantDone: true, wantReason: stopReasonRankingExhausted},
		{name: "analysis flagged nothing", stopRanking: true, at: afterAnalysis, wantDone: true, wantReason: stopReasonNothingFlagged},
		{name: "Jev answered nothing (#86)", stopRanking: true, at: afterAnalysis, state: analystDown, wantDone: true,
			wantReason: stopReasonAnalysisUnavailable + "analyst: " + jevOutOfCredits, wantFailure: "analyst: " + jevOutOfCredits},
		{name: "a failed reviewer is not an unavailable analysis", stopRanking: true, at: afterAnalysis, state: reviewerDown, wantDone: true, wantReason: stopReasonNothingFlagged},
		{name: "cycle under every bound continues", at: afterCycle, state: CampaignState{CandidatesTried: 1}},
		{name: "candidate budget spent", at: afterCycle, state: CampaignState{CandidatesTried: 4}, wantDone: true, wantReason: stopReasonMaxCandidates},
		{name: "optimizer down two cycles", at: afterCycle, state: optimizerDown, wantDone: true,
			wantReason: stopReasonProviderFailure + "optimizer: HTTP 402", wantFailure: "optimizer: HTTP 402"},
		{name: "optimizer down one cycle", at: afterCycle, state: optimizerOnce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &campaignGraph{cfg: Config{MaxCandidates: 4, MaxConsecutiveFailures: 3, StopWhenRankingExhausted: tc.stopRanking}}
			end, done := g.ended(tc.state, tc.at)
			if done != tc.wantDone || end.reason != tc.wantReason || end.failure != tc.wantFailure {
				t.Errorf("ended = %+v, %v; want {reason:%q failure:%q}, %v", end, done, tc.wantReason, tc.wantFailure, tc.wantDone)
			}
		})
	}
}

// TestUnavailableAnalysisFailsTheCampaign runs the graph the way
// daily-2026-10-03-heldout-tomlv ran: Jev answered for no hot function, the
// ranking was empty, and the campaign recorded `completed` with "the analysis
// flagged no target". It must stop before any candidate and fail, naming the
// analyst.
func TestUnavailableAnalysisFailsTheCampaign(t *testing.T) {
	orch, optimizerCalls := exhaustionGraph(t, &fakeCauseAnalyst{err: errors.New(jevOutOfCredits)}, true)
	result := runUntilNode[CampaignResult](t, orch, "optimizer-test", "user-1", "session-analysis-down", causeCampaign, "finalize_campaign")
	if result.CandidatesTried != 0 || *optimizerCalls != 0 {
		t.Fatalf("candidates = %d, optimizer calls = %d, want none", result.CandidatesTried, *optimizerCalls)
	}
	if !strings.HasPrefix(result.StopReason, stopReasonAnalysisUnavailable) {
		t.Errorf("stop reason = %q, want the analysis named unavailable", result.StopReason)
	}
	if !strings.HasPrefix(result.ProviderFailure, "analyst: ") {
		t.Errorf("provider failure = %q, want the analyst's failure", result.ProviderFailure)
	}
}
