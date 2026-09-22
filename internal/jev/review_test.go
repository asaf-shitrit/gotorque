package jev

import (
	"strings"
	"testing"
)

// TestReviewBaselineMatchesQuestions pins the review baseline to the text it
// was measured with, as TestBaselineMatchesQuestions does for causes.
func TestReviewBaselineMatchesQuestions(t *testing.T) {
	if got := reviewDigest(); got != reviewBaselineDigest {
		t.Fatalf("hazard questions or review state changed (digest %s, baseline measured against %s): "+
			"re-measure with TestLiveReviewBaseline and update review_baseline.go", got, reviewBaselineDigest)
	}
}

func TestEveryHazardHasAQuestionAConcernACheckAndABaseline(t *testing.T) {
	questions := ReviewQuestions()
	for _, h := range Hazards {
		checkHazard(t, h, questions[string(h)])
	}
}

func checkHazard(t *testing.T, h Hazard, q Question) {
	t.Helper()
	if q.Type != "boolean" || !strings.Contains(q.Instructions, "`patch`") {
		t.Errorf("%s question = %+v, want a yes/no question naming `patch`", h, q)
	}
	if h.Concern() == "" || h.Check() == "" {
		t.Errorf("%s has no concern or check", h)
	}
	if b := reviewBaseline[h]; b.std <= 0 || b.mean <= 0 || b.mean >= 1 {
		t.Errorf("%s baseline = %+v", h, b)
	}
}

func TestReviewStateCarriesOnlyTheChange(t *testing.T) {
	state := ReviewState("h", "f", "src", "diff")
	if len(state) != 5 || state["patch"] != "diff" || state["source"] != "src" {
		t.Errorf("review state = %v", state)
	}
}

func hazardAnswers(p map[Hazard]float64) map[string]Answer {
	answers := map[string]Answer{}
	for _, h := range Hazards {
		prob, ok := p[h]
		if !ok {
			prob = reviewBaseline[h].mean
		}
		answers[string(h)] = Answer{Type: "boolean", Probability: prob}
	}
	return answers
}

// TestFlaggedHazardsNeedsBothDistanceAndAYes: an answer far above a question's
// usual one but still below one half is Jev leaning no, and a yes that is
// ordinary for its question is no signal; only both together raise a concern.
func TestFlaggedHazardsNeedsBothDistanceAndAYes(t *testing.T) {
	flagged := FlaggedHazards([]HazardScore{
		{Hazard: HazardDroppedError, Probability: 0.96, Z: 5.0},
		{Hazard: HazardOutputOrder, Probability: 0.30, Z: 9.0},
		{Hazard: HazardOffTarget, Probability: 0.55, Z: 1.2},
	})
	if len(flagged) != 1 || flagged[0].Hazard != HazardDroppedError {
		t.Errorf("flagged = %v, want only the dropped error", flagged)
	}
}

// TestRankHazardsOrdersByDistanceFromTheUsualAnswer: with today's baseline a
// yes on any question is already more than two standard deviations out.
func TestRankHazardsOrdersByDistanceFromTheUsualAnswer(t *testing.T) {
	scores, err := RankHazards(hazardAnswers(map[Hazard]float64{HazardDroppedError: 0.96, HazardOutputOrder: 0.30}))
	if err != nil {
		t.Fatal(err)
	}
	if scores[0].Hazard != HazardOutputOrder || scores[1].Hazard != HazardDroppedError {
		t.Errorf("ranking = %v, want output_order (furthest out) then dropped_error", scores[:2])
	}
	for _, h := range Hazards {
		if b := reviewBaseline[h]; b.mean+HazardThreshold*b.std >= HazardFloor {
			t.Errorf("%s: a yes is not automatically unusual (mean %.2f, sd %.2f); revisit the flag rule", h, b.mean, b.std)
		}
	}
}

func TestRankHazardsRejectsIncompleteOrImpossibleAnswers(t *testing.T) {
	missing := hazardAnswers(nil)
	delete(missing, string(HazardConcurrency))
	if _, err := RankHazards(missing); err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Errorf("missing answer: err = %v", err)
	}
	if _, err := RankHazards(hazardAnswers(map[Hazard]float64{HazardNumericOutput: -0.1})); err == nil {
		t.Error("accepted a negative probability")
	}
}
