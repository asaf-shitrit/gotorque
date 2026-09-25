package jev

import (
	"slices"
	"strings"
	"testing"
)

// TestBaselineMatchesQuestions pins the baseline to the text it was measured
// with. Rewording a question or the state template moves Jev's answers, so the
// old means would silently skew every ranking.
func TestBaselineMatchesQuestions(t *testing.T) {
	if got := digest(); got != baselineDigest {
		t.Fatalf("question set or state template changed (digest %s, baseline measured against %s): "+
			"re-measure with TestLiveBaseline and update baseline.go", got, baselineDigest)
	}
}

func TestEveryCauseHasAQuestionAndABaseline(t *testing.T) {
	questions := Questions()
	for _, cause := range Causes {
		q, ok := questions[string(cause)]
		if !ok || q.Type != "boolean" || q.Criteria["true"] == "" || q.Criteria["false"] == "" {
			t.Errorf("%s question = %+v", cause, q)
		}
		if b := baseline[cause]; b.std <= 0 || b.mean <= 0 || b.mean >= 1 {
			t.Errorf("%s baseline = %+v", cause, b)
		}
		if cause.Summary() == "" {
			t.Errorf("%s has no summary", cause)
		}
	}
}

func answersAt(p map[Cause]float64) map[string]Answer {
	answers := map[string]Answer{}
	for _, cause := range Causes {
		prob, ok := p[cause]
		if !ok {
			prob = baseline[cause].mean
		}
		answers[string(cause)] = Answer{Type: "boolean", Probability: prob}
	}
	return answers
}

// TestRankComparesAnswersAgainstTheirOwnBaseline is the reason Rank exists: the
// allocation question answers yes to most functions, so its raw probability
// would win even when another question is the one that stands out.
func TestRankComparesAnswersAgainstTheirOwnBaseline(t *testing.T) {
	scores, err := Rank(answersAt(map[Cause]float64{CauseAlloc: 0.70, CauseUnbufferedIO: 0.60}))
	if err != nil {
		t.Fatal(err)
	}
	if scores[0].Cause != CauseUnbufferedIO || scores[1].Cause != CauseAlloc {
		t.Fatalf("ranking = %v, want unbuffered IO ahead of the higher raw alloc answer", scores)
	}
	if scores[0].Probability != 0.60 || scores[0].Z < 2 {
		t.Errorf("top score = %+v", scores[0])
	}
}

func TestRankBreaksTiesInCauseOrder(t *testing.T) {
	scores, err := Rank(answersAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]Cause, 0, len(scores))
	for _, s := range scores {
		got = append(got, s.Cause)
	}
	if !slices.Equal(got, Causes) {
		t.Errorf("tied ranking = %v, want %v", got, Causes)
	}
}

func TestRankRejectsIncompleteOrImpossibleAnswers(t *testing.T) {
	missing := answersAt(nil)
	delete(missing, string(CauseRedundant))
	if _, err := Rank(missing); err == nil || !strings.Contains(err.Error(), "redundant") {
		t.Errorf("missing answer: err = %v", err)
	}
	if _, err := Rank(answersAt(map[Cause]float64{CausePrealloc: 1.5})); err == nil {
		t.Error("accepted a probability above one")
	}
}

func TestFlaggedKeepsAtMostTwoCausesAboveTheThreshold(t *testing.T) {
	scores := []Score{{Cause: CauseUnbufferedIO, Probability: 0.9, Z: 3}, {Cause: CauseAlloc, Probability: 0.8, Z: 1}, {Cause: CausePrealloc, Probability: 0.7, Z: 0.8}, {Cause: CauseRedundant, Probability: 0.6, Z: 0.1}}
	flagged := Flagged(scores)
	if len(flagged) != MaxFlagged || flagged[0].Cause != CauseUnbufferedIO || flagged[1].Cause != CauseAlloc {
		t.Errorf("flagged = %v", flagged)
	}
	if got := Flagged([]Score{{Cause: CauseAlloc, Probability: 0.9, Z: FlagThreshold - 0.01}}); len(got) != 0 {
		t.Errorf("flagged a cause below the threshold: %v", got)
	}
}

// TestFlagNeedsJevToSayYes: a cause far above Jev's usual answer is still not
// flagged when Jev's own answer is below the floor, and it does not stop a
// lower cause that clears both gates (ADR 0025).
func TestFlagNeedsJevToSayYes(t *testing.T) {
	scores := []Score{{Cause: CauseAlloc, Probability: 0.45, Z: 2.5}, {Cause: CauseRedundant, Probability: 0.7, Z: 1.2}}
	got := Flag(scores, nil)
	if len(got) != 1 || got[0].Cause != CauseRedundant {
		t.Errorf("flagged = %v, want only redundant", got)
	}
}

// TestFlagSkipsExcludedCauses: a cause the caller excludes frees its slot.
func TestFlagSkipsExcludedCauses(t *testing.T) {
	scores := []Score{{Cause: CauseUnbufferedIO, Probability: 0.9, Z: 3}, {Cause: CauseAlloc, Probability: 0.8, Z: 1}, {Cause: CauseRedundant, Probability: 0.7, Z: 0.9}}
	got := Flag(scores, func(c Cause) bool { return c == CauseUnbufferedIO })
	if len(got) != 2 || got[0].Cause != CauseAlloc || got[1].Cause != CauseRedundant {
		t.Errorf("flagged = %v, want alloc then redundant", got)
	}
}

// TestFastPathIsDeferredNotDropped: fast_path clears the same gates and is
// kept beside the two primary causes, for the targets' final tier.
func TestFastPathIsDeferredNotDropped(t *testing.T) {
	scores := []Score{{Cause: CauseFastPath, Probability: 0.8, Z: 4}, {Cause: CauseAlloc, Probability: 0.8, Z: 1}, {Cause: CauseRedundant, Probability: 0.7, Z: 0.9}}
	got := Flag(scores, nil)
	if len(got) != 3 || got[0].Cause != CauseFastPath || got[1].Cause != CauseAlloc || got[2].Cause != CauseRedundant {
		t.Errorf("flagged = %v, want fast_path kept alongside two primary causes", got)
	}
	if !Deferred(CauseFastPath) || Deferred(CauseAlloc) {
		t.Error("only fast_path is deferred")
	}
}

func TestRemedyNamesTheSite(t *testing.T) {
	got := CauseUnbufferedIO.Remedy("gron", "main.go:207")
	if !strings.Contains(got, "gron (main.go:207)") || !strings.Contains(got, "bufio") {
		t.Errorf("remedy = %q", got)
	}
}

// TestSiteStateCarriesNoProfile guards the finding that a profile in the state
// pulls every answer toward whatever that profile is dominated by.
func TestSiteStateCarriesNoProfile(t *testing.T) {
	state := SiteState("a.go", "func f() {}")
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"context", "file", "source"}) {
		t.Errorf("state keys = %v", keys)
	}
}
