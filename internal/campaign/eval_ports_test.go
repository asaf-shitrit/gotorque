package campaign

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
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

	require.True(t, engine.confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))

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
