package orchestrator

import "github.com/asaf-shitrit/gotorque/internal/domain"

// A campaign's ledger is its candidate history in attempt order: the
// candidates a caller recorded before this process (CampaignRequest.
// RecordedCandidates), then those the graph judges. It is the one source of
// the counters the stop bounds read and of the targets already tried, so a
// resumed campaign derives the numbers an uninterrupted one would have
// reached instead of a caller copying each counter across. Copying them one
// field at a time is how a resume came to restart the candidate budget and
// the attempt numbers at zero (#91).

// tallies are the counters the stop bounds read.
type tallies struct {
	tried                   int
	consecutiveFailures     int
	consecutiveInconclusive int
}

// talliesOf folds a ledger into its tallies. tried is the highest attempt
// rather than a count, so the next attempt number never repeats a recorded
// one.
func talliesOf(ledger []PriorCandidate, separateInconclusiveBound bool) tallies {
	var t tallies
	for _, c := range ledger {
		t.tried = max(t.tried, c.Attempt)
		t = t.after(domain.Decision(c.Decision), separateInconclusiveBound)
	}
	return t
}

// after is the tallies once one more verdict is counted. An accepted
// candidate clears both streaks. A rejection counts as a failure and breaks
// any run of unresolved verdicts. An inconclusive verdict counts on its own
// streak when the campaign configured one, and otherwise counts as a failure,
// which is the default behavior.
func (t tallies) after(decision domain.Decision, separateInconclusiveBound bool) tallies {
	switch decision {
	case domain.DecisionAccepted:
		t.consecutiveFailures, t.consecutiveInconclusive = 0, 0
	case domain.DecisionRejected:
		t.consecutiveFailures++
		t.consecutiveInconclusive = 0
	case domain.DecisionInconclusive:
		t.consecutiveInconclusive++
		if !separateInconclusiveBound {
			t.consecutiveFailures++
		}
	}
	return t
}
