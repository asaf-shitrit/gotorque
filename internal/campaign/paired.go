package campaign

import (
	"math"
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/domain"
)

// Test names recorded on a comparison (domain.MetricComparison.Test), so a
// verdict says which test its significance came from (ADR 0048).
const (
	// testSignedRank is the Wilcoxon signed-rank test on the per-pair
	// differences of an interleaved series.
	testSignedRank = "wilcoxon_signed_rank"
	// testWelch is the two-sample Welch-style |t| over unpaired columns, used
	// when the two series cannot be paired by index.
	testWelch = "welch"
)

// pairedAlpha is the two-sided p-value below which a signed-rank test counts a
// difference. It matches benchstatAlpha and the |t| > 2.2 of the unpaired
// path, both about p < 0.05.
const pairedAlpha = 0.05

// exactSignedRankLimit is the largest pair count whose signed-rank p-value is
// computed from the exact distribution of the rank sum. Above it, or with
// tied magnitudes, the normal approximation with a tie correction is used.
const exactSignedRankLimit = 100

// pairedValues returns the usable baseline and candidate values of two series
// that were measured as interleaved pairs, in pair order and equal in length.
// RunInterleaved appends one result to each side per pair, so index i of both
// series is one pair. A pair with an unusable value on either side is dropped
// whole, which keeps the columns aligned. ok is false when the series differ
// in length, so they were not measured as pairs and the caller must treat them
// as two independent samples.
func pairedValues(baseline, candidate []domain.RunResult, selector metricSelector) (base, cand []float64, ok bool) {
	if len(baseline) != len(candidate) {
		return nil, nil, false
	}
	base = make([]float64, 0, len(baseline))
	cand = make([]float64, 0, len(candidate))
	for i := range baseline {
		b, okBase := selector(baseline[i])
		c, okCand := selector(candidate[i])
		if !okBase || !okCand || !finiteValue(b) || !finiteValue(c) {
			continue
		}
		base = append(base, b)
		cand = append(cand, c)
	}
	return base, cand, true
}

func finiteValue(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// pairDifferences is candidate minus baseline for each pair.
func pairDifferences(baseline, candidate []float64) []float64 {
	diffs := make([]float64, len(baseline))
	for i := range baseline {
		diffs[i] = candidate[i] - baseline[i]
	}
	return diffs
}

// signedRankP is the two-sided p-value of the Wilcoxon signed-rank test that
// the per-pair differences are symmetric about zero. Pairs with a zero
// difference carry no information and are dropped, as in the usual
// convention. With no pair left the result is 1.
//
// The test reads the sign and the rank of each difference, not the size of the
// raw values, so a slow stretch of the machine that moves both halves of a pair
// together cancels instead of widening the noise (ADR 0048). It assumes the
// differences are independent and symmetric about their centre under no
// effect. The ABBA order balances the position effect between baseline-first
// and candidate-first pairs, which keeps the null symmetric to within one pair.
func signedRankP(diffs []float64) float64 {
	magnitudes := make([]float64, 0, len(diffs))
	signs := make([]bool, 0, len(diffs))
	for _, d := range diffs {
		if d == 0 || !finiteValue(d) {
			continue
		}
		magnitudes = append(magnitudes, math.Abs(d))
		signs = append(signs, d > 0)
	}
	n := len(magnitudes)
	if n == 0 {
		return 1
	}
	ranks, tied := averageRanks(magnitudes)
	positive := 0.0
	for i, isPositive := range signs {
		if isPositive {
			positive += ranks[i]
		}
	}
	if !tied && n <= exactSignedRankLimit {
		return exactSignedRankP(n, int(math.Round(positive)))
	}
	return approxSignedRankP(n, positive, ranks)
}

// averageRanks ranks values from 1 with ties sharing the mean of their ranks,
// and reports whether any tie occurred.
func averageRanks(values []float64) (ranks []float64, tied bool) {
	order := make([]int, len(values))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		switch {
		case values[a] < values[b]:
			return -1
		case values[a] > values[b]:
			return 1
		}
		return 0
	})
	ranks = make([]float64, len(values))
	for start := 0; start < len(order); {
		end := start + 1
		for end < len(order) && values[order[end]] == values[order[start]] {
			end++
		}
		if end-start > 1 {
			tied = true
		}
		mean := float64(start+1+end) / 2 // mean of ranks start+1 .. end
		for _, idx := range order[start:end] {
			ranks[idx] = mean
		}
		start = end
	}
	return ranks, tied
}

// exactSignedRankP is the exact two-sided p-value for n untied nonzero
// differences whose positive ranks sum to w: twice the smaller tail of the
// distribution of the rank sum over all 2^n sign assignments.
func exactSignedRankP(n, w int) float64 {
	top := n * (n + 1) / 2
	// ways[s] counts the sign assignments whose positive ranks sum to s.
	ways := make([]float64, top+1)
	ways[0] = 1
	for rank := 1; rank <= n; rank++ {
		for s := top; s >= rank; s-- {
			ways[s] += ways[s-rank]
		}
	}
	total := math.Ldexp(1, n)
	lower, upper := 0.0, 0.0
	for s, count := range ways {
		if s <= w {
			lower += count
		}
		if s >= w {
			upper += count
		}
	}
	return math.Min(1, 2*math.Min(lower, upper)/total)
}

// approxSignedRankP is the normal approximation with a continuity correction
// and the variance reduced for tied magnitudes.
func approxSignedRankP(n int, positive float64, ranks []float64) float64 {
	count := float64(n)
	mu := count * (count + 1) / 4
	variance := count * (count + 1) * (2*count + 1) / 24
	variance -= tieCorrection(ranks) / 48
	if variance <= 0 {
		return 1
	}
	z := (math.Abs(positive-mu) - 0.5) / math.Sqrt(variance)
	if z <= 0 {
		return 1
	}
	return math.Erfc(z / math.Sqrt2)
}

// tieCorrection is the sum of t^3 - t over each group of tied ranks.
func tieCorrection(ranks []float64) float64 {
	groups := map[float64]float64{}
	for _, r := range ranks {
		groups[r]++
	}
	sum := 0.0
	for _, size := range groups {
		sum += size*size*size - size
	}
	return sum
}

// pairedSignificant reports whether the signed-rank test counts the pairs'
// differences, with its p-value. Fewer than four pairs never count, as with
// the unpaired path.
func pairedSignificant(baseline, candidate []float64) (significant bool, p float64) {
	if len(baseline) < 4 {
		return false, 1
	}
	p = signedRankP(pairDifferences(baseline, candidate))
	return p < pairedAlpha, p
}

// pairedSupport is metricSupport over pairs: the same two kinds of evidence,
// with significance read from the signed-rank test and the equivalence
// interval built from the standard error of the per-pair differences, so
// drift between pairs no more widens the interval than it widens the test. An
// improvement gets no equivalence support (see metricSupport).
func pairedSupport(baseline, candidate []float64, baselineMean, deltaPercent, limit float64) bool {
	if len(baseline) < 4 {
		return false
	}
	if significant, _ := pairedSignificant(baseline, candidate); significant {
		return true
	}
	diffs := pairDifferences(baseline, candidate)
	centre, _ := mean(diffs)
	se := math.Sqrt(variance(diffs) / float64(len(diffs)))
	if se == 0 {
		return centre == 0 // exact identical measurements
	}
	if deltaPercent < 0 {
		return false // an improvement needs significance, not equivalence
	}
	if baselineMean <= 0 || deltaPercent >= limit {
		return false
	}
	return (centre+welchSignificance*se)/baselineMean*100 < limit
}
