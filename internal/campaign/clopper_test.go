package campaign

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClopperPearsonUpperBound(t *testing.T) {
	// Reference values from the defining binomial tail: the p where the chance
	// of at most hits hits in runs is 0.05.
	cases := []struct {
		hits, runs int
		want       float64
	}{
		{0, 70, 0.041893},
		{1, 70, 0.065984},
		{3, 50, 0.147837},
		{10, 100, 0.163718},
		{0, 12, 0.220922},
	}
	for _, c := range cases {
		require.InDelta(t, c.want, clopperPearsonUpper(c.hits, c.runs), 1e-6, "%d of %d", c.hits, c.runs)
	}
	require.InDelta(t, 1-math.Pow(0.05, 1.0/70), clopperPearsonUpper(0, 70), 1e-12)
	// A rate with no room above it, and a rate with no runs at all, both bound
	// at one; nonsense below zero reads as no hits.
	require.InDelta(t, 1.0, clopperPearsonUpper(5, 5), 1e-12)
	require.InDelta(t, 1.0, clopperPearsonUpper(0, 0), 1e-12)
	require.InDelta(t, clopperPearsonUpper(0, 20), clopperPearsonUpper(-1, 20), 1e-12)
}

func TestBetaQuantileBracketsTheProbability(t *testing.T) {
	for _, params := range [][2]float64{{1, 70}, {4, 47}, {11, 90}} {
		x := betaQuantile(0.95, params[0], params[1])
		require.InDelta(t, 0.95, regularizedBeta(params[0], params[1], x), 1e-9)
	}
	// The regularized function is clamped at the ends of its domain, and the
	// median of the uniform Beta(1, 1) is one half.
	require.InDelta(t, 0.0, regularizedBeta(2, 3, 0), 1e-12)
	require.InDelta(t, 1.0, regularizedBeta(2, 3, 1), 1e-12)
	require.InDelta(t, 0.5, betaQuantile(0.5, 1, 1), 1e-9)
	require.InDelta(t, 1e-300, nonZero(0), 1e-310)
	require.InDelta(t, 1.0, nonZero(1), 1e-12)
}
