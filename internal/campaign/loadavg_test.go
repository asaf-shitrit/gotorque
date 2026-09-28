package campaign

import (
	"context"
	"strings"
	"testing"
	"time"

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

// scriptedLoad returns the given load samples in turn, then the last one.
func scriptedLoad(loads ...float64) func() []float64 {
	i := 0
	return func() []float64 {
		l := loads[min(i, len(loads)-1)]
		i++
		return []float64{l}
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestQuietWaiterWaitsUntilTheLoadFalls(t *testing.T) {
	w := quietWaiter{sample: scriptedLoad(9, 8, 2), cpus: 8, limit: time.Minute, poll: 10 * time.Second, sleep: noSleep}
	waited, expired := w.wait(context.Background())
	require.Equal(t, 20*time.Second, waited)
	require.False(t, expired)
}

func TestQuietWaiterGivesUpAtItsLimit(t *testing.T) {
	w := quietWaiter{sample: scriptedLoad(9), cpus: 8, limit: 30 * time.Second, poll: 10 * time.Second, sleep: noSleep}
	waited, expired := w.wait(context.Background())
	require.Equal(t, 30*time.Second, waited)
	require.True(t, expired)
}

func TestQuietWaiterDoesNotWaitOnAQuietMachine(t *testing.T) {
	w := quietWaiter{sample: scriptedLoad(1), cpus: 8, limit: time.Minute, poll: time.Second, sleep: func(context.Context, time.Duration) error { panic("slept") }}
	waited, expired := w.wait(context.Background())
	require.Zero(t, waited)
	require.False(t, expired)
}

func TestQuietWaiterStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := quietWaiter{sample: scriptedLoad(9), cpus: 8, limit: time.Hour, poll: time.Hour, sleep: sleepContext}
	_, expired := w.wait(ctx)
	require.True(t, expired)
	require.Equal(t, quietWaitLimit, defaultQuietWaiter().limit)
}

func TestReportNamesTheQuietWait(t *testing.T) {
	var waited strings.Builder
	writeCandidateLoad(&waited, CandidateRecord{QuietWait: 40 * time.Second})
	require.Contains(t, waited.String(), "Waited 40s for the machine to go quiet before measuring, until it was quiet")

	var gaveUp strings.Builder
	writeCandidateLoad(&gaveUp, CandidateRecord{QuietWait: 3 * time.Minute, QuietWaitExpired: true, LoadAverages: []float64{9, 9}, LoadContended: true})
	require.Contains(t, gaveUp.String(), "gave up")
	require.Contains(t, gaveUp.String(), "contended")
}

func TestReportNamesADiscardedPass(t *testing.T) {
	var b strings.Builder
	writeCandidateLoad(&b, CandidateRecord{DiscardedLoad: []float64{6.4, 16.58}, LoadAverages: []float64{3.1, 3.4}})
	require.Contains(t, b.String(), "- Measured twice: the first pass ended contended (load 6.40 -> 16.58) and was discarded")
	require.Contains(t, b.String(), "3.10 -> 3.40")
	require.NotContains(t, b.String(), "contended: load above")
}
