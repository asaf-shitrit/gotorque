package orchestrator

import "github.com/asaf-shitrit/gotorque/internal/agents"

const (
	stopReasonMaxCandidates = "maximum candidate count reached"
	// stopReasonRankingExhausted ends a campaign whose analysis has no untried
	// target left (Config.StopWhenRankingExhausted).
	stopReasonRankingExhausted = "every target the analysis flagged has been tried"
	// stopReasonNothingFlagged ends a campaign whose analysis flagged no
	// target at all. live1-tengo reported the exhausted reason above for a
	// ranking that had nothing in it, which read as if targets had been tried.
	stopReasonNothingFlagged      = "the analysis flagged no target"
	stopReasonConsecutiveFailures = "consecutive rejection/inconclusive limit reached"
	// stopReasonConsecutiveInconclusive is reported only when the campaign
	// configures stop_after_inconclusive, which bounds unresolved verdicts
	// separately from failures.
	stopReasonConsecutiveInconclusive = "consecutive inconclusive limit reached"
	// stopReasonProviderFailure is followed by the last failure of the cycle
	// that tripped it.
	stopReasonProviderFailure = "model provider unavailable: the optimizer failed in two consecutive cycles; last failure: "
	// stopReasonAnalysisUnavailable is followed by the analyst's failure.
	stopReasonAnalysisUnavailable = "analysis unavailable: the analyst answered for no hot function; failure: "
)

// outageCycles is how many consecutive cycles the optimizer, the one model
// role, must fail before the breaker stops the campaign. One cycle is the
// single call the degrading wrapper already absorbs: once Jev and code served
// every other role, one slow cycle, its three attempts each cut while still
// producing, ended a gojq campaign two candidates early. A revoked key or an
// empty balance still stops the campaign quickly, since those HTTP statuses
// are not retried.
const outageCycles = 2

// checkpoint is where in a campaign the graph asks whether it has ended.
type checkpoint int

const (
	// atStart runs before the first cycle of a process, so a resumed campaign
	// already at a bound stops without spending a candidate.
	atStart checkpoint = iota
	// afterAnalysis runs once the analysis is merged and code has planned the
	// cycle's target.
	afterAnalysis
	// afterCycle runs once the cycle's verdict is recorded.
	afterCycle
)

// ending is how a campaign stops: the reason, and the failure that made it
// stop when nothing it depends on could answer. A non-empty failure means the
// campaign failed instead of completing, because no bound was reached.
type ending struct {
	reason  string
	failure string
}

// ended decides whether the campaign stops at this checkpoint, and how. It is
// the one place a stop reason or a failure is chosen; the graph only routes
// on it and the caller only reads it from CampaignResult. The decision is
// code's, from the cycle's recorded facts, never a model's.
//
// Two ways of stopping are failures rather than completions. An optimizer
// that failed in outageCycles consecutive cycles is a provider outage. An
// analyst that answered for no hot function, in a campaign that stops when
// its ranking is empty, is an unavailable analysis: reported as "flagged no
// target", it read as a finding about the target. Jev and the optimizer are
// served from one OpenRouter account (ADR 0030), so an empty balance takes
// out both, and on 2026-10-03 the campaign whose first 402 came from Jev was
// the only one recorded as completed (#86).
func (g *campaignGraph) ended(state CampaignState, at checkpoint) (ending, bool) {
	switch at {
	case atStart:
		if reason, hit := g.consecutiveBound(state); hit {
			return ending{reason: reason}, true
		}
	case afterAnalysis:
		return g.endedAfterAnalysis(state)
	case afterCycle:
		if failure, down := providerFailure(state); down && state.OutageCycles >= outageCycles {
			return ending{reason: stopReasonProviderFailure + failure, failure: failure}, true
		}
		if reason := g.stopReason(state); reason != "" {
			return ending{reason: reason}, true
		}
	}
	return ending{}, false
}

// endedAfterAnalysis stops a campaign whose analysis left the optimizer no
// target, when the campaign stops on an empty ranking. Without that stop the
// optimizer takes free choice, so a failed analyst only degrades the cycle.
func (g *campaignGraph) endedAfterAnalysis(state CampaignState) (ending, bool) {
	if !g.cfg.StopWhenRankingExhausted || state.Target != nil {
		return ending{}, false
	}
	if len(state.Analysis.Targets) > 0 {
		return ending{reason: stopReasonRankingExhausted}, true
	}
	if cause := roleFailure(state.CycleFailures, string(agents.RoleAnalyst)); cause != "" {
		failure := string(agents.RoleAnalyst) + ": " + cause
		return ending{reason: stopReasonAnalysisUnavailable + failure, failure: failure}, true
	}
	return ending{reason: stopReasonNothingFlagged}, true
}

// providerFailure reports the cycle's last optimizer failure when the
// optimizer failed in it. Only the optimizer counts toward an outage: a Jev
// role that fails degrades its own output, and a Jev outage is caught where it
// leaves the campaign with nothing to do (endedAfterAnalysis).
func providerFailure(state CampaignState) (string, bool) {
	if cause := roleFailure(state.CycleFailures, string(agents.RoleOptimizer)); cause != "" {
		return string(agents.RoleOptimizer) + ": " + cause, true
	}
	return "", false
}

// stopReason reports which bound the campaign has reached, or "". The
// candidate budget is checked first so a campaign that used its last patch
// reports that rather than a streak bound it also happens to meet.
func (g *campaignGraph) stopReason(state CampaignState) string {
	if state.CandidatesTried >= g.cfg.MaxCandidates {
		return stopReasonMaxCandidates
	}
	if reason, hit := g.consecutiveBound(state); hit {
		return reason
	}
	return ""
}

// consecutiveBound reports which consecutive-verdict bound a campaign has
// reached, if any. A campaign that configured stop_after_inconclusive tracks
// unresolved verdicts on their own; otherwise they extend the failure streak,
// which is the historical behavior.
func (g *campaignGraph) consecutiveBound(state CampaignState) (string, bool) {
	if g.cfg.MaxConsecutiveInconclusive > 0 && state.ConsecutiveInconclusive >= g.cfg.MaxConsecutiveInconclusive {
		return stopReasonConsecutiveInconclusive, true
	}
	if state.ConsecutiveFailures >= g.cfg.MaxConsecutiveFailures {
		return stopReasonConsecutiveFailures, true
	}
	return "", false
}
