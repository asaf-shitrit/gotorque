package campaign

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The power simulation behind ADR 0048. It draws 25 interleaved pairs the way
// RunInterleaved measures them and asks each test whether it sees a -3% effect
// (power) and whether it sees one when the binaries are identical (false
// positives). Run the full table with:
//
//	GOTORQUE_POWER_TRIALS=4000 go test ./internal/campaign -run TestPowerOfPairedAndUnpairedTests -v
//
// The default trial count keeps the suite fast and still separates the tests
// by far more than its sampling error.

const (
	simPairs        = 25
	simEffect       = -0.03
	simOrderEffect  = 0.0266 // PR #31: the second run of a pair ran 2.66% slower
	simDriftRho     = 0.9
	simDriftStdDev  = 0.06
	simWithinPair   = 0.02
	simIIDNoise     = 0.07
	simDefaultTrial = 400
)

// powerCell is one noise model: the spread shared by both halves of a pair
// (drift), the spread each run has on its own, and the position effect.
type powerCell struct {
	name   string
	drift  float64
	noise  float64
	second float64
}

var powerCells = []powerCell{
	{"iid 7% noise", 0, simIIDNoise, 0},
	{"6% drift, 2% within-pair noise", simDriftStdDev, simWithinPair, 0},
	{"6% drift, 2% noise, 2.66% order effect", simDriftStdDev, simWithinPair, simOrderEffect},
}

// simulatedPairs draws baseline and candidate columns for one series.
func simulatedPairs(rng *rand.Rand, cell powerCell, effect float64) (base, cand []float64) {
	base, cand = make([]float64, simPairs), make([]float64, simPairs)
	drift := rng.NormFloat64() * cell.drift
	innovation := cell.drift * math.Sqrt(1-simDriftRho*simDriftRho)
	for i := range simPairs {
		drift = simDriftRho*drift + innovation*rng.NormFloat64()
		first, second := drift+cell.noise*rng.NormFloat64(), drift+cell.second+cell.noise*rng.NormFloat64()
		baseLog, candLog := first, second+effect
		if i%2 == 1 { // candidate first on odd pairs, as RunInterleaved orders them
			baseLog, candLog = second, first+effect
		}
		base[i], cand[i] = 10e6*math.Exp(baseLog), 10e6*math.Exp(candLog)
	}
	return base, cand
}

// mannWhitneyP is the two-sided Mann-Whitney U p-value by the normal
// approximation. benchstat's default test is this one, applied to the two
// columns as if they were independent.
func mannWhitneyP(a, b []float64) float64 {
	n, m := float64(len(a)), float64(len(b))
	pooled := append(slices.Clone(a), b...)
	ranks, _ := averageRanks(pooled)
	rankSum := 0.0
	for _, r := range ranks[:len(a)] {
		rankSum += r
	}
	u := rankSum - n*(n+1)/2
	z := (math.Abs(u-n*m/2) - 0.5) / math.Sqrt(n*m*(n+m+1)/12)
	return math.Erfc(max(z, 0) / math.Sqrt2)
}

// powerRow holds how often each test calls a series significant.
type powerRow struct{ mannWhitney, welch, signedRank float64 }

func simulatePower(cell powerCell, effect float64, trials int, seed uint64) powerRow {
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	var hits [3]int
	for range trials {
		base, cand := simulatedPairs(rng, cell, effect)
		if mannWhitneyP(base, cand) < 0.05 {
			hits[0]++
		}
		if statisticallySupported(base, cand) {
			hits[1]++
		}
		if significant, _ := pairedSignificant(base, cand); significant {
			hits[2]++
		}
	}
	rate := func(h int) float64 { return float64(h) / float64(trials) }
	return powerRow{rate(hits[0]), rate(hits[1]), rate(hits[2])}
}

func simulationTrials(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("GOTORQUE_POWER_TRIALS"); v != "" {
		trials, err := strconv.Atoi(v)
		require.NoError(t, err)
		return trials
	}
	return simDefaultTrial
}

func TestPowerOfPairedAndUnpairedTests(t *testing.T) {
	trials := simulationTrials(t)
	var table strings.Builder
	fmt.Fprintf(&table, "\n%d trials per cell, %d pairs, effect %+.0f%% (power) and 0%% (false positives), alpha 0.05\n", trials, simPairs, simEffect*100)
	fmt.Fprintf(&table, "%-42s %-9s %12s %12s %12s\n", "cell", "effect", "Mann-Whitney", "Welch |t|>2.2", "signed-rank")
	rows := map[string][2]powerRow{}
	for i, cell := range powerCells {
		seed := uint64(i + 1)
		win, null := simulatePower(cell, simEffect, trials, seed), simulatePower(cell, 0, trials, seed+100)
		rows[cell.name] = [2]powerRow{win, null}
		fmt.Fprintf(&table, "%-42s %-9s %11.1f%% %12.1f%% %11.1f%%\n", cell.name, "-3%", 100*win.mannWhitney, 100*win.welch, 100*win.signedRank)
		fmt.Fprintf(&table, "%-42s %-9s %11.1f%% %12.1f%% %11.1f%%\n", "", "none", 100*null.mannWhitney, 100*null.welch, 100*null.signedRank)
	}
	t.Log(table.String())

	iid, drift, order := rows[powerCells[0].name], rows[powerCells[1].name], rows[powerCells[2].name]
	// Drift is where the unpaired tests lose and the paired test does not.
	require.Greater(t, drift[0].signedRank, 0.95)
	require.Less(t, drift[0].mannWhitney, 0.85)
	require.Less(t, drift[0].welch, 0.85)
	require.Greater(t, order[0].signedRank, 0.9)
	require.Less(t, order[0].mannWhitney, 0.8)
	// With no drift pairing gives nothing away: the tests are within sampling
	// error of each other.
	require.InDelta(t, iid[0].mannWhitney, iid[0].signedRank, 0.1)
	// No test calls identical binaries different more than about 5% of the time.
	for _, cell := range powerCells {
		require.Less(t, rows[cell.name][1].signedRank, 0.09, cell.name)
	}
}
