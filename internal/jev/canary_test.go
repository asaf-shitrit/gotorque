package jev

import "testing"

// TestCanaryMatchesQuestions pins canaryRecorded to the exact question set
// and state it was measured with, the same way TestBaselineMatchesQuestions
// pins baseline.go. Rewording a canary question or canaryFunction moves Jev's
// answers, so the old recorded values would silently stop meaning anything.
func TestCanaryMatchesQuestions(t *testing.T) {
	if got := checkCanaryDigest(); got != canaryDigest {
		t.Fatalf("canary question set or state changed (digest %s, canary measured against %s): "+
			"re-measure with TestLiveCanary and update canary.go", got, canaryDigest)
	}
}

func TestCanaryAsksEveryCause(t *testing.T) {
	questions := canaryQuestionSet()
	if len(questions) != len(Causes) {
		t.Fatalf("canary question count = %d, want %d", len(questions), len(Causes))
	}
	for _, cause := range canaryCauses {
		if _, ok := canaryRecorded[string(cause)]; !ok {
			t.Errorf("canary question %s has no recorded value", cause)
		}
	}
}

func TestCanaryDriftFlagsAMovedAnswer(t *testing.T) {
	answers := map[string]Answer{}
	for id, want := range canaryRecorded {
		answers[id] = Answer{Type: "noul", Probability: want}
	}
	if drift := canaryDrift(answers); len(drift) != 0 {
		t.Fatalf("drift = %v, want none for the recorded values themselves", drift)
	}
	moved := string(canaryCauses[0])
	answers[moved] = Answer{Type: "noul", Probability: canaryRecorded[moved] + 0.2}
	drift := canaryDrift(answers)
	if len(drift) != 1 {
		t.Fatalf("drift = %v, want exactly one flagged question", drift)
	}
}

func TestCanaryDriftFlagsAMissingAnswer(t *testing.T) {
	answers := map[string]Answer{}
	for id, want := range canaryRecorded {
		answers[id] = Answer{Type: "noul", Probability: want}
	}
	delete(answers, string(canaryCauses[0]))
	drift := canaryDrift(answers)
	if len(drift) != 1 {
		t.Fatalf("drift = %v, want the missing question flagged", drift)
	}
}

func TestCanaryDriftToleratesSmallMovement(t *testing.T) {
	answers := map[string]Answer{}
	for id, want := range canaryRecorded {
		answers[id] = Answer{Type: "noul", Probability: want}
	}
	moved := string(canaryCauses[0])
	answers[moved] = Answer{Type: "noul", Probability: canaryRecorded[moved] + canaryTolerance(moved)/2}
	if drift := canaryDrift(answers); len(drift) != 0 {
		t.Fatalf("drift = %v, want movement inside tolerance ignored", drift)
	}
}

func TestCanaryToleranceWidensForLooseAnswers(t *testing.T) {
	for _, cause := range canaryCauses {
		id := string(cause)
		if _, ok := canarySpread[id]; !ok {
			t.Errorf("canary question %s has no recorded spread", id)
		}
		if got := canaryTolerance(id); got < CanaryTolerance {
			t.Errorf("tolerance for %s = %.3f, below the floor", id, got)
		}
	}
	if got := canaryTolerance("superlinear"); got <= CanaryTolerance {
		t.Errorf("superlinear tolerance = %.3f, want it widened past %.2f by its sd", got, CanaryTolerance)
	}
	if got := canaryTolerance("redundant"); got != CanaryTolerance {
		t.Errorf("redundant tolerance = %.3f, want the %.2f floor", got, CanaryTolerance)
	}
}
