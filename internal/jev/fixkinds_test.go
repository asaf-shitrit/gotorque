package jev

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKindBaselineMatchesQuestions pins the fix-kind baseline to the exact
// text it was measured with, as TestBaselineMatchesQuestions does for causes.
func TestKindBaselineMatchesQuestions(t *testing.T) {
	require.Equal(t, kindBaselineDigest, kindDigest(), "fix-kind questions or state template changed: re-measure the baseline")
}

func TestEveryKindHasAQuestionAndABaseline(t *testing.T) {
	questions := KindQuestions()
	require.Len(t, questions, len(FixKinds))
	for _, k := range FixKinds {
		s := kindSpecs[k]
		require.Equal(t, "boolean", questions[string(k)].Type, k)
		require.Positive(t, kindBaseline[k].std, k)
		if s.chosen {
			require.Contains(t, k.Remedy("f", "a.go:1"), "f (a.go:1)", k)
		}
	}
	require.True(t, HasKinds(CauseAlloc))
	require.False(t, HasKinds(CauseFastPath), "asked, but worse than guessing on held-out fixes, so never chosen")
	require.True(t, HasKinds(CauseStringBuild))
	require.False(t, HasKinds(CauseRedundant), "asked, but near chance, so never chosen")
	require.False(t, HasKinds(CauseUnbufferedIO))
}

func kindAnswers(p map[FixKind]float64) map[string]Answer {
	answers := map[string]Answer{}
	for _, k := range FixKinds {
		answers[string(k)] = Answer{Type: "boolean", Probability: kindBaseline[k].mean}
	}
	for k, v := range p {
		answers[string(k)] = Answer{Type: "boolean", Probability: v}
	}
	return answers
}

func TestChooseKindNeedsAClearLeader(t *testing.T) {
	kind, scores, ok, err := ChooseKind(CauseStringBuild, kindAnswers(map[FixKind]float64{KindDropFmt: 0.6}))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, KindDropFmt, kind)
	require.Len(t, scores, 2)

	// Both at their usual answer: no leader.
	_, _, ok, err = ChooseKind(CauseStringBuild, kindAnswers(nil))
	require.NoError(t, err)
	require.False(t, ok)

	// A leader below its own usual answer is not chosen, however far ahead.
	_, _, ok, err = ChooseKind(CauseStringBuild, kindAnswers(map[FixKind]float64{KindDropFmt: 0.1, KindBuilder: 0.0}))
	require.NoError(t, err)
	require.False(t, ok)

	// Fast path is asked about but never chosen.
	_, _, ok, err = ChooseKind(CauseFastPath, kindAnswers(map[FixKind]float64{KindASCII: 0.99}))
	require.NoError(t, err)
	require.False(t, ok)

	// Redundant work is asked about but never chosen.
	_, _, ok, err = ChooseKind(CauseRedundant, kindAnswers(map[FixKind]float64{KindComputeOnce: 0.99}))
	require.NoError(t, err)
	require.False(t, ok)

	missing := kindAnswers(nil)
	delete(missing, string(KindBuilder))
	_, _, _, err = ChooseKind(CauseStringBuild, missing)
	require.ErrorContains(t, err, "no answer to the sb_builder question")
}
