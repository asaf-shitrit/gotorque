package campaign

import (
	"math"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/stretchr/testify/require"
)

// bruteSignedRankP enumerates every sign assignment of ranks 1..n, the
// definition the dynamic program in exactSignedRankP must reproduce.
func bruteSignedRankP(n, w int) float64 {
	lower, upper := 0, 0
	for mask := 0; mask < 1<<n; mask++ {
		sum := 0
		for r := 0; r < n; r++ {
			if mask&(1<<r) != 0 {
				sum += r + 1
			}
		}
		if sum <= w {
			lower++
		}
		if sum >= w {
			upper++
		}
	}
	return math.Min(1, 2*float64(min(lower, upper))/float64(int(1)<<n))
}

func TestExactSignedRankMatchesTheDefinition(t *testing.T) {
	for n := 1; n <= 12; n++ {
		for w := 0; w <= n*(n+1)/2; w++ {
			require.InDelta(t, bruteSignedRankP(n, w), exactSignedRankP(n, w), 1e-12, "n=%d w=%d", n, w)
		}
	}
	// Five differences all of one sign: 2 / 2^5, the textbook smallest p at n = 5.
	require.InDelta(t, 0.0625, exactSignedRankP(5, 15), 1e-12)
}

func TestSignedRankP(t *testing.T) {
	cases := []struct {
		name  string
		diffs []float64
		want  float64
		delta float64
	}{
		{"nothing to test", nil, 1, 0},
		{"zeros carry no information", []float64{0, 0, 0}, 1, 0},
		{"five of one sign", []float64{1, 2, 3, 4, 5}, 0.0625, 1e-12},
		{"ten of one sign", []float64{-1, -2, -3, -4, -5, -6, -7, -8, -9, -10}, 2.0 / 1024, 1e-12},
		{"balanced signs", []float64{1, -2, 3, -4, 5, -6, 7, -8}, bruteSignedRankP(8, 16), 1e-12},
		{"zeros are dropped before ranking", []float64{0, 1, 2, 3, 4, 5}, 0.0625, 1e-12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want, signedRankP(tc.diffs), tc.delta)
		})
	}
}

// TestTiedMagnitudesUseTheNormalApproximation: ties break the exact
// distribution, so the p-value comes from the corrected normal. Forty pairs of
// +1, with two -1, are overwhelmingly one-sided.
func TestTiedMagnitudesUseTheNormalApproximation(t *testing.T) {
	diffs := make([]float64, 40)
	for i := range diffs {
		diffs[i] = 1
	}
	diffs[0], diffs[1] = -1, -1
	require.Less(t, signedRankP(diffs), 1e-6)

	// All magnitudes equal and signs split evenly: no evidence either way.
	even := make([]float64, 40)
	for i := range even {
		even[i] = float64(1 - 2*(i%2))
	}
	require.InDelta(t, 1, signedRankP(even), 1e-9)
}

// TestTheApproximationAgreesWithTheExactTestWhereVerdictsTurn: near the exact
// limit the two answers must be close around p = 0.05, or a verdict would
// depend on which side of the switch the series length falls.
func TestTheApproximationAgreesWithTheExactTestWhereVerdictsTurn(t *testing.T) {
	const n = 60
	ranks := make([]float64, n)
	for i := range ranks {
		ranks[i] = float64(i + 1)
	}
	checked := 0
	for w := 0; w <= n*(n+1)/2; w++ {
		exact := exactSignedRankP(n, w)
		if exact < 0.02 || exact > 0.2 {
			continue
		}
		checked++
		require.InDelta(t, exact, approxSignedRankP(n, float64(w), ranks), 0.1*exact, "w=%d", w)
	}
	require.Positive(t, checked)
}

func runsOf(metric string, values ...float64) []domain.RunResult {
	return pooledAsRuns(values, metric)
}

func TestPairedValuesKeepWholePairsOnly(t *testing.T) {
	base := runsOf("wall_time_ns", 10, math.NaN(), 12, 13)
	cand := runsOf("wall_time_ns", 9, 8, math.Inf(1), 11)
	b, c, ok := pairedValues(base, cand, wallTime)
	require.True(t, ok)
	require.Equal(t, []float64{10, 13}, b)
	require.Equal(t, []float64{9, 11}, c)

	_, _, ok = pairedValues(base, cand[:3], wallTime)
	require.False(t, ok, "series of different lengths were not measured as pairs")
}

// driftingPairs builds n pairs whose machine speed wanders by tens of percent
// between pairs while the candidate is faster by the same few percent within
// every pair: the case the unpaired test is blind to.
func driftingPairs(n int, effect float64) (base, cand []domain.RunResult) {
	b := make([]float64, n)
	c := make([]float64, n)
	for i := range b {
		drift := 1 + 0.3*math.Sin(float64(i)*0.9)*math.Cos(float64(i)*0.31)
		wobble := 1 + 0.004*float64((i*7)%5-2)
		b[i] = 10e6 * drift
		c[i] = b[i] * (1 + effect) * wobble
	}
	return runsOf("wall_time_ns", b...), runsOf("wall_time_ns", c...)
}

func TestACompareMetricJudgesPairsOnTheirDifferences(t *testing.T) {
	base, cand := driftingPairs(25, -0.03)
	got := compareMetric("w", "wall_time_ns", "ns", 2, base, cand, wallTime)[0]
	require.Equal(t, testSignedRank, got.Test)
	require.True(t, got.Significant, "p=%v", got.PValue)
	require.True(t, got.StatisticallyFit)
	require.Less(t, got.PValue, 0.001)

	// The same samples as two independent columns carry the drift in their
	// variance, which is what the Welch-style fallback still reports.
	unpairedBase, unpairedCand := collectMetric(base, wallTime), collectMetric(cand, wallTime)
	require.False(t, statisticallySupported(unpairedBase, unpairedCand))
}

func TestACompareMetricFallsBackToWelchForSeriesThatAreNotPairs(t *testing.T) {
	base := runsOf("wall_time_ns", 100, 101, 99, 100, 102, 98)
	cand := runsOf("wall_time_ns", 90, 91, 89, 90, 92)
	got := compareMetric("w", "wall_time_ns", "ns", 2, base, cand, wallTime)[0]
	require.Equal(t, testWelch, got.Test)
	require.Zero(t, got.PValue)
	require.True(t, got.Significant)
}

func TestACompareMetricWithTooFewSamplesRecordsNoTest(t *testing.T) {
	got := compareMetric("w", "wall_time_ns", "ns", 2, runsOf("wall_time_ns", 1, 2, 3), runsOf("wall_time_ns", 1, 2, 3), wallTime)[0]
	require.Empty(t, got.Test)
	require.False(t, got.Significant)
}

// TestAPairedNullIsNotSignificant: byte-identical series with a position
// effect that ABBA balances, the A/A case the harness's null candidates
// measure. The second run of every pair is slower; alternating which binary
// goes second must keep that out of the verdict.
func TestAPairedNullIsNotSignificant(t *testing.T) {
	const n = 40
	b := make([]float64, n)
	c := make([]float64, n)
	for i := range b {
		noise := 1 + 0.01*float64((i*37)%11-5)/5
		first, second := 10e6*noise, 10e6*noise*1.0266
		if i%2 == 0 { // baseline first
			b[i], c[i] = first, second
		} else {
			b[i], c[i] = second, first
		}
	}
	got := compareMetric("w", "wall_time_ns", "ns", 2, runsOf("wall_time_ns", b...), runsOf("wall_time_ns", c...), wallTime)[0]
	require.False(t, got.Significant, "p=%v", got.PValue)
}

func TestAPairedEquivalenceIntervalStillSupportsAFlatReading(t *testing.T) {
	base := runsOf("peak_memory_bytes", 100, 100, 100, 100, 100, 100)
	cand := runsOf("peak_memory_bytes", 100, 100.5, 99.5, 100, 100.2, 99.8)
	got := compareMetric("", "peak_memory_bytes", "bytes", 2, base, cand, peakMemory)[0]
	require.False(t, got.Significant)
	require.True(t, got.StatisticallyFit, "a flat reading is supported by its interval")

	cand = runsOf("peak_memory_bytes", 105, 106, 104, 105, 107, 103)
	got = compareMetric("", "peak_memory_bytes", "bytes", 2, base, cand, peakMemory)[0]
	require.True(t, got.Significant)
}
