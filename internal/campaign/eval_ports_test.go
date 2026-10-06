package campaign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// recordedEvent is one event a recordingJournal saw.
type recordedEvent struct {
	kind, message string
}

// recordingJournal is a journal that keeps what it is told in memory, so a
// test can assert what an evaluation reported without reading a store.
type recordingJournal struct {
	events []recordedEvent
	notes  []string
}

func (j *recordingJournal) event(kind, message string, _ any) error {
	j.events = append(j.events, recordedEvent{kind, message})
	return nil
}

func (j *recordingJournal) isolationNotes(notes []string) { j.notes = append(j.notes, notes...) }

// TestEvaluationReportsThroughItsJournal: a confirmation series reports its
// event to the journal it was given, not to the campaign's store.
func TestEvaluationReportsThroughItsJournal(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	journal := &recordingJournal{}
	engine.journal = journal
	stored, err := engine.store.Events()
	require.NoError(t, err)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}

	require.True(t, engine.campaignEvaluator().confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))

	require.Len(t, journal.events, 1)
	require.Equal(t, "measurement_confirmed", journal.events[0].kind)
	require.Contains(t, journal.events[0].message, "without significance after 25 pairs")
	after, err := engine.store.Events()
	require.NoError(t, err)
	require.Len(t, after, len(stored), "nothing reached the campaign's store")
}

func TestTheCampaignsJournalPersistsEvents(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	require.NoError(t, engine.evalJournal().event("probe", "persisted", nil))
	engine.evalJournal().isolationNotes([]string{"degraded"})

	events, err := engine.store.Events()
	require.NoError(t, err)
	require.Equal(t, "probe", events[len(events)-1].Type)
	require.Equal(t, []string{"degraded"}, engine.state.SandboxIsolationNotes)
}

// scriptedMachine is a machine whose load answers are scripted: successive
// load() calls return successive entries, the last repeating.
type scriptedMachine struct {
	loads   [][]float64
	limit   float64
	waited  time.Duration
	expired bool

	sampled, waits int
}

func (m *scriptedMachine) load() []float64 {
	i := min(m.sampled, len(m.loads)-1)
	m.sampled++
	return m.loads[i]
}

func (m *scriptedMachine) contended(loads []float64) bool {
	for _, l := range loads {
		if l > m.limit {
			return true
		}
	}
	return false
}

func (m *scriptedMachine) waitQuiet(context.Context) (time.Duration, bool) {
	m.waits++
	return m.waited, m.expired
}

// countingCandidate replaces the candidate binary with a script that appends
// to a counter file on each execution and then runs the baseline binary, so
// its output still matches and a test can count how often it ran. runs reads
// the count and reset starts it over.
func countingCandidate(t *testing.T, engine *Engine, candidate string) (runs func() int, reset func()) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "runs")
	script := "#!/bin/sh\necho x >> " + strconv.Quote(counter) + "\nexec " + strconv.Quote(engine.state.BinaryPath) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(candidate, []byte(script), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	runs = func() int {
		data, err := os.ReadFile(counter)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(data), "x")
	}
	reset = func() { require.NoError(t, os.RemoveAll(counter)) }
	return runs, reset
}

// TestAContendedFirstPassIsDiscardedAndMeasuredOnce: the guard against false
// accepts from a load burst mid-measurement (overnight-miller-1) had no test,
// because the load average was a process global. With a scripted machine, a
// first pass that ends contended is thrown away and measured again from
// scratch, once; a second pass that is still contended is kept and flagged,
// not measured a third time.
func TestAContendedFirstPassIsDiscardedAndMeasuredOnce(t *testing.T) {
	engine, _, _, candidate := measuredEngine(t)
	engine.state.LocalIsolation = true
	runs, reset := countingCandidate(t, engine, candidate)
	s := engine.campaignSettings()
	// What measureAndFinalize hands measureSeedsOnQuietMachine: the load it
	// sampled before measuring and the quiet wait it spent.
	opening := func() *orchestrator.CandidateEvidence {
		return &orchestrator.CandidateEvidence{LoadAverages: []float64{1}, QuietWait: 3 * time.Second}
	}
	measure := func(evidence *orchestrator.CandidateEvidence) *measurement {
		m := &measurement{}
		require.True(t, engine.campaignEvaluator().measureSeedsOnQuietMachine(context.Background(), s, evidence, "candidate", candidate, m), evidence.Summary)
		return m
	}

	quiet := &scriptedMachine{loads: [][]float64{{2}}, limit: 5}
	engine.machine = quiet
	evidence := opening()
	measure(evidence)
	onePass := runs()
	require.Positive(t, onePass)
	require.Zero(t, quiet.waits, "a quiet machine is not waited for again")
	require.Empty(t, evidence.DiscardedLoad)
	require.Equal(t, 3*time.Second, evidence.QuietWait)

	// The load burst arrives while the first pass runs: the sample taken when
	// it ends is contended, the one after the second wait is not.
	reset()
	burst := &scriptedMachine{loads: [][]float64{{9}, {2}}, limit: 5, waited: 20 * time.Second}
	engine.machine = burst
	evidence = opening()
	m := measure(evidence)
	require.Equal(t, 2*onePass, runs(), "the contended pass was measured again, once")
	require.Equal(t, 1, burst.waits)
	require.Equal(t, 23*time.Second, evidence.QuietWait, "the second wait is added to the first")
	require.Equal(t, []float64{1, 9}, evidence.DiscardedLoad, "the discarded pass's load is kept beside the record")
	require.Equal(t, []float64{2}, evidence.LoadAverages, "the record reports the second pass's load")
	require.Len(t, m.seeds, 1)
	require.Len(t, m.seeds[0].ab.Baseline, measurementRepetitions, "only the second pass's samples reach the verdict")

	// Still contended on the second pass: it is kept as it is, not repeated.
	reset()
	stuck := &scriptedMachine{loads: [][]float64{{9}}, limit: 5, waited: 30 * time.Second, expired: true}
	engine.machine = stuck
	evidence = opening()
	m = measure(evidence)
	require.Equal(t, 2*onePass, runs(), "a second contended pass is not followed by a third")
	require.True(t, evidence.QuietWaitExpired)
	require.Len(t, m.seeds[0].ab.Baseline, measurementRepetitions)
}

// TestMeasurementFlagsContentionFromTheMachine: the record carries the loads
// the machine reported before and after the measurement, and flags the
// candidate when either was contended.
func TestMeasurementFlagsContentionFromTheMachine(t *testing.T) {
	engine, _, _, candidate := measuredEngine(t)
	engine.state.LocalIsolation = true
	engine.machine = &scriptedMachine{loads: [][]float64{{1}, {2}, {9}}, limit: 5}
	evidence := &orchestrator.CandidateEvidence{}

	require.True(t, engine.campaignEvaluator().measureAndFinalize(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate))

	require.Equal(t, []float64{1, 9}, evidence.LoadAverages)
	require.True(t, evidence.LoadContended)
}

// TestScratchBaselineNarrowsWithoutTouchingTheCampaigns: both adapters narrow
// the required passes and spend a re-check, but only the campaign's reaches the
// persisted state.
func TestScratchBaselineNarrowsWithoutTouchingTheCampaigns(t *testing.T) {
	state := State{BaselineTestPasses: []string{"p::A", "p::B"}, BaselineTestFailures: []string{"p::F"}, BaselineUnbuildable: []string{"q"}}

	scratch := scratchBaselineFrom(campaignBaseline{&state})
	scratch.requirePasses([]string{"p::A"})
	scratch.spendRecheck()
	require.Equal(t, []string{"p::A"}, scratch.passes())
	require.Equal(t, 1, scratch.rechecks())
	require.Equal(t, []string{"p::F"}, scratch.failures())
	require.Equal(t, []string{"q"}, scratch.unbuildable())
	require.Equal(t, []string{"p::A", "p::B"}, state.BaselineTestPasses)
	require.Zero(t, state.BaselineRechecks)

	campaign := campaignBaseline{&state}
	campaign.requirePasses([]string{"p::A"})
	campaign.spendRecheck()
	require.Equal(t, []string{"p::A"}, state.BaselineTestPasses)
	require.Equal(t, 1, state.BaselineRechecks)
}

// campaignEvaluator is the evaluator a campaign attempt would use, built from
// the engine as it stands now, for tests that drive one stage directly.
func (e *Engine) campaignEvaluator() *evaluator { return e.newEvaluator(e.campaignSettings()) }
