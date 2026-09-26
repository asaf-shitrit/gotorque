package jev

import "fmt"

// canaryFunction is a fixed, synthetic Go function the preflight asks Jev
// about on every run. It is not real target code; it exists only so the same
// questions can be asked against the same bytes every time, giving a
// behavioral signal that a version change would move even when
// GET /typesafe/v1/models' release_date does not (see checkModelRelease and
// ADR 0029).
const canaryFunction = `func renderEntries(r io.Reader, w io.Writer, sorted bool) (int, error) {
	entries, err := readEntries(r)
	if err != nil {
		return 1, fmt.Errorf("read entries: %s", err)
	}
	if sorted {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	}
	var lines []string
	for _, e := range entries {
		line := e.Key + " = " + strconv.Quote(e.Value)
		if e.Deprecated {
			line = fmt.Sprintf("%s // deprecated", line)
		}
		lines = append(lines, line)
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return 0, nil
}`

// canaryCauses asks all seven cause questions (causes.go) about
// canaryFunction. Reusing the production question text, rather than
// writing separate canary wording, means a reworded cause question already
// forces a re-measure here through the same digest guard, instead of two
// question sets silently drifting apart.
var canaryCauses = Causes

// canaryState is the fixed state sent with every canary question.
var canaryState = map[string]string{"context": stateContext, "file": "canary.go", "source": canaryFunction}

// canaryQuestionSet returns the fixed question set asked against canaryState.
func canaryQuestionSet() map[string]Question {
	questions := make(map[string]Question, len(canaryCauses))
	for _, cause := range canaryCauses {
		questions[string(cause)] = specs[cause].question
	}
	return questions
}

// canaryRecorded is Jev's answer to each canary question, measured live
// against TypeSafe's own serving (see TestLiveCanary). It is a single
// snapshot, not a distribution like baseline.go: the canary's job is to
// notice the model moving, not to rank anything, so one recorded value per
// question is enough to compare against.
//
// Valid only for canaryFunction and canaryQuestionSet exactly as they stand;
// TestCanaryMatchesQuestions fails on any edit to either. Re-measure with
// TestLiveCanary rather than pasting new numbers by hand.
// Measured over 6 repeats, 2026-09-26 (per-answer sd 0.006–0.015).
// canaryFunction is chosen so most answers sit mid-range: an answer pinned
// near 0 or 1 barely moves when the model changes, so it would make a poor
// drift signal.
var canaryRecorded = map[string]float64{
	"alloc":         0.8500,
	"unbuffered_io": 0.6600,
	"string_build":  0.7400,
	"fast_path":     0.4233,
	"superlinear":   0.2467,
	"prealloc":      0.7767,
	"redundant":     0.1600,
}

// CanaryTolerance is how far a canary answer may move from canaryRecorded
// before the preflight treats it as drift. TypeSafe reports a per-question sd
// of 0.0102 (ADR 0012 measured 0.01 independently, and the canary's own
// answers ranged 0.006–0.015), so 0.05 is at least three standard deviations: comfortably past ordinary sampling noise, and a
// version change that shifts the cause baselines will almost certainly move
// at least one of the seven canary answers past it.
const CanaryTolerance = 0.05

const canaryDigest = "8980f0361b576c1ee564dce3e174d2e6465280685c04dc6bbf243497d67e14f7"

// checkCanaryDigest reports whether the canary question set and state still
// match what canaryRecorded was measured against.
func checkCanaryDigest() string {
	return digestPayload(map[string]any{"state": canaryState, "questions": canaryQuestionSet()})
}

// canaryDrift compares one evaluate response's canary answers against
// canaryRecorded and reports, in canaryCauses order, every question that
// moved by more than CanaryTolerance and any question canaryRecorded expects
// but the response did not answer.
func canaryDrift(answers map[string]Answer) []string {
	var drift []string
	for _, cause := range canaryCauses {
		id := string(cause)
		want, ok := canaryRecorded[id]
		if !ok {
			continue
		}
		got, answered := answers[id]
		if !answered {
			drift = append(drift, id+": no answer")
			continue
		}
		if delta := got.Probability - want; delta > CanaryTolerance || -delta > CanaryTolerance {
			drift = append(drift, fmt.Sprintf("%s: %.4f, recorded %.4f (Δ%.4f)", id, got.Probability, want, delta))
		}
	}
	return drift
}
