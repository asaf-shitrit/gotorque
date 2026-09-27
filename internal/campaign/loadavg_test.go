package campaign

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContended(t *testing.T) {
	require.False(t, contended(nil, 8))
	require.False(t, contended([]float64{1.2, 3.9}, 8))
	require.True(t, contended([]float64{1.2, 7.8}, 8))
	require.True(t, contended([]float64{2.1}, 2))
}

func TestFixedLoad(t *testing.T) {
	l, ok := fixedLoad(3*2048, 2048)
	require.True(t, ok)
	require.InDelta(t, 3.0, l, 1e-9)
	_, ok = fixedLoad(1, 0)
	require.False(t, ok)
}

func TestParseProcLoadavg(t *testing.T) {
	l, ok := parseProcLoadavg("7.80 5.10 3.00 2/812 4242\n")
	require.True(t, ok)
	require.InDelta(t, 7.8, l, 1e-9)
	_, ok = parseProcLoadavg("")
	require.False(t, ok)
	_, ok = parseProcLoadavg("x 1 2")
	require.False(t, ok)
}

func TestSampleLoadOnThisPlatform(t *testing.T) {
	loads := sampleLoad()
	if len(loads) == 0 {
		t.Skip("platform exposes no load average")
	}
	require.GreaterOrEqual(t, loads[0], 0.0)
	require.Positive(t, machineCPUs())
}

func TestReportFlagsAContendedMeasurement(t *testing.T) {
	var b strings.Builder
	writeCandidateLoad(&b, CandidateRecord{LoadAverages: []float64{1.25, 7.8}, LoadContended: true})
	require.Contains(t, b.String(), "- Load average during measurement: 1.25 -> 7.80")
	require.Contains(t, b.String(), "contended")

	var clean strings.Builder
	writeCandidateLoad(&clean, CandidateRecord{LoadAverages: []float64{1.25, 1.5}})
	require.NotContains(t, clean.String(), "contended")

	var none strings.Builder
	writeCandidateLoad(&none, CandidateRecord{})
	require.Empty(t, none.String())
}
