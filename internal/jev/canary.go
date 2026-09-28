package jev

import "fmt"

// canaryFunction is a fixed, synthetic Go function the preflight asks Jev
// about on every run. It is not real target code; it exists only so the same
// questions can be asked against the same bytes every time, giving a
// behavioral signal that TypeSafe's build can move behind an id gotorque
// still pins exactly (see ADR 0030).
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
// against the pinned build (jev.Model) served by TypeSafe (see TestLiveCanary).
// It is a single snapshot, not a distribution like baseline.go: the canary's
// job is to notice the model moving, not to rank anything, so one recorded
// value per question is enough to compare against.
//
// Valid only for canaryFunction and canaryQuestionSet exactly as they stand;
// TestCanaryMatchesQuestions fails on any edit to either. Re-measure with
// TestLiveCanary rather than pasting new numbers by hand.
// Measured over 20 repeats, 2026-09-28, against the pinned build (jev.Model)
// through OpenRouter's System One API (ADR 0030). canaryFunction is chosen so
// most answers sit mid-range: an answer pinned near 0 or 1 barely moves when
// the model changes, so it would make a poor drift signal.
var canaryRecorded = map[string]float64{
	"alloc":         0.8495,
	"unbuffered_io": 0.6540,
	"string_build":  0.7365,
	"fast_path":     0.4165,
	"superlinear":   0.2435,
	"prealloc":      0.7610,
	"redundant":     0.1600,
}

// canarySpread is each answer's standard deviation over the same 20 repeats.
// The answers are not equally steady: superlinear's sd was 0.026 and
// redundant's 0.006, so one tolerance for all seven was either blind on the
// steady ones or noisy on the loose ones. With a flat 0.05, superlinear
// failed the preflight twice in one night on the pinned build (0.17, then a
// median of 0.19, against a recorded 0.248 that sat at the high end of its
// own range).
var canarySpread = map[string]float64{
	"alloc":         0.0080,
	"unbuffered_io": 0.0198,
	"string_build":  0.0168,
	"fast_path":     0.0203,
	"superlinear":   0.0257,
	"prealloc":      0.0151,
	"redundant":     0.0063,
}

// CanaryTolerance is the least a canary answer may move before the preflight
// treats it as drift; canaryTolerance widens it to canarySpreads standard
// deviations for a question whose answers vary more. Together with the
// median-of-three confirmation, a model change that moves the cause answers
// by more than their noise still fails, and ordinary sampling does not.
const (
	CanaryTolerance = 0.05
	canarySpreads   = 4
)

func canaryTolerance(id string) float64 {
	return max(CanaryTolerance, canarySpreads*canarySpread[id])
}

const canaryDigest = "182d11791110d3da035f4f5913a9fbeffcae5f0cd2dcc932fb3ace35ae558711"

// checkCanaryDigest reports whether the canary question set and state still
// match what canaryRecorded was measured against.
func checkCanaryDigest() string {
	return digestPayload(map[string]any{"state": canaryState, "questions": canaryQuestionSet()})
}

// canaryDrift compares one evaluate response's canary answers against
// canaryRecorded and reports, in canaryCauses order, every question that
// moved by more than its canaryTolerance and any question canaryRecorded
// expects but the response did not answer.
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
		if delta, tol := got.Probability-want, canaryTolerance(id); delta > tol || -delta > tol {
			drift = append(drift, fmt.Sprintf("%s: %.4f, recorded %.4f (Δ%.4f)", id, got.Probability, want, delta))
		}
	}
	return drift
}
