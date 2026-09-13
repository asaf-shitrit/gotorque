package campaign

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
)

func TestCandidateEventSummaryNamesPrimaryMetricAndReason(t *testing.T) {
	record := CandidateRecord{
		Attempt:  3,
		Decision: domain.DecisionRejected,
		Comparisons: []domain.MetricComparison{
			{Name: "cpu_time_ns", DeltaPercent: -4.5},
			{Name: manifest.DefaultPrimaryMetric, DeltaPercent: -1.254},
		},
		Reasons: []string{"wall_time_ns improved 1.25% but\nneeds 3.00%"},
	}
	got := candidateEventSummary(record)
	want := "attempt 3: rejected — wall_time_ns -1.25% (unsupported): wall_time_ns improved 1.25% but needs 3.00%"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestCandidateEventSummaryFallsBackToFirstComparison(t *testing.T) {
	record := CandidateRecord{
		Attempt:     1,
		Decision:    domain.DecisionAccepted,
		Comparisons: []domain.MetricComparison{{Name: "peak_memory_bytes", DeltaPercent: 0.5, StatisticallyFit: true}},
	}
	got := candidateEventSummary(record)
	if !strings.HasPrefix(got, "attempt 1: accepted — peak_memory_bytes +0.50% (supported)") {
		t.Fatalf("summary = %q", got)
	}
}

func TestCandidateEventSummaryBoundsMultilineReasons(t *testing.T) {
	record := CandidateRecord{
		Attempt:  2,
		Decision: domain.DecisionRejected,
		Reasons:  []string{strings.Repeat("patch does not apply\n", 40)},
	}
	got := candidateEventSummary(record)
	if strings.Contains(got, "\n") {
		t.Fatalf("summary retains a newline: %q", got)
	}
	prefix := "attempt 2: rejected: "
	if utf8.RuneCountInString(got) > len(prefix)+maxEventReasonChars+1 {
		t.Fatalf("summary runes = %d", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated summary lacks an ellipsis: %q", got)
	}
}

// A running campaign holds the database's exclusive lock, which is why the
// report command used to answer "timeout" for the entire duration of a run.
func TestLoadReportFallsBackToSnapshotWhileDatabaseIsLocked(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// The snapshot is decoded through the manifest types, so it has to carry a
	// manifest with the positive durations the loader guarantees in practice.
	state := State{
		ID:     "snapshot-id",
		Status: StatusRunning,
		Manifest: manifest.Manifest{Campaign: manifest.CampaignLimits{
			MaxDuration:           manifest.Duration(time.Minute),
			DiscoveryStallTimeout: manifest.Duration(time.Minute),
			MinimumCommandTimeout: manifest.Duration(time.Second),
		}},
	}
	if err := WriteReports(dir, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReport(dir)
	if err != nil {
		t.Fatalf("LoadReport on a locked campaign: %v", err)
	}
	if loaded.ID != "snapshot-id" {
		t.Fatalf("state.ID = %q, want the snapshot's id", loaded.ID)
	}
}
