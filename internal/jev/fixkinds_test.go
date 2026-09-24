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
	require.True(t, HasKinds(CauseFastPath))
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
	kind, scores, ok, err := ChooseKind(CauseFastPath, kindAnswers(map[FixKind]float64{KindASCII: 0.6}))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, KindASCII, kind)
	require.Len(t, scores, 2)

	// Both at their usual answer: no leader.
	_, _, ok, err = ChooseKind(CauseFastPath, kindAnswers(nil))
	require.NoError(t, err)
	require.False(t, ok)

	// A leader below its own usual answer is not chosen, however far ahead.
	_, _, ok, err = ChooseKind(CauseFastPath, kindAnswers(map[FixKind]float64{KindASCII: 0.1, KindCommonCase: 0.0}))
	require.NoError(t, err)
	require.False(t, ok)

	// Redundant work is asked about but never chosen.
	_, _, ok, err = ChooseKind(CauseRedundant, kindAnswers(map[FixKind]float64{KindComputeOnce: 0.99}))
	require.NoError(t, err)
	require.False(t, ok)

	missing := kindAnswers(nil)
	delete(missing, string(KindASCII))
	_, _, _, err = ChooseKind(CauseFastPath, missing)
	require.ErrorContains(t, err, "no answer to the fp_ascii question")
}
