package orchestrator

import (
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/domain"
)

func ledgerOf(decisions ...domain.Decision) []PriorCandidate {
	out := make([]PriorCandidate, len(decisions))
	for i, d := range decisions {
		out[i] = PriorCandidate{Attempt: i + 1, Decision: string(d)}
	}
	return out
}

// TestTalliesOf pins the fold a resumed campaign derives its counters from.
// It must match what counting the same verdicts one cycle at a time reaches.
func TestTalliesOf(t *testing.T) {
	acc, rej, inc := domain.DecisionAccepted, domain.DecisionRejected, domain.DecisionInconclusive
	gapped := ledgerOf(rej, rej)
	gapped[1].Attempt = 5
	for _, tc := range []struct {
		name     string
		ledger   []PriorCandidate
		separate bool
		want     tallies
	}{
		{name: "empty", want: tallies{}},
		{name: "rejections build the failure streak", ledger: ledgerOf(rej, rej, rej), want: tallies{tried: 3, consecutiveFailures: 3}},
		{name: "an accept clears both streaks", ledger: ledgerOf(rej, inc, acc, rej), want: tallies{tried: 4, consecutiveFailures: 1}},
		{name: "inconclusive counts as a failure by default", ledger: ledgerOf(inc, inc), want: tallies{tried: 2, consecutiveFailures: 2, consecutiveInconclusive: 2}},
		{name: "a separate inconclusive bound keeps its own streak", ledger: ledgerOf(rej, inc, inc), separate: true, want: tallies{tried: 3, consecutiveFailures: 1, consecutiveInconclusive: 2}},
		{name: "tried is the highest attempt, so numbers never repeat", ledger: gapped, want: tallies{tried: 5, consecutiveFailures: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := talliesOf(tc.ledger, tc.separate); got != tc.want {
				t.Errorf("talliesOf = %+v, want %+v", got, tc.want)
			}
			state := CampaignState{}
			for _, c := range tc.ledger {
				countDecision(&state, domain.Decision(c.Decision), tc.separate)
			}
			if state.ConsecutiveFailures != tc.want.consecutiveFailures || state.ConsecutiveInconclusive != tc.want.consecutiveInconclusive {
				t.Errorf("cycle-by-cycle streaks = %d/%d, want the fold's %d/%d", state.ConsecutiveFailures, state.ConsecutiveInconclusive, tc.want.consecutiveFailures, tc.want.consecutiveInconclusive)
			}
		})
	}
}
