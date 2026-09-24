package campaign

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
)

func TestRenderMarkdownStatesWhatTheBehaviorGateVerified(t *testing.T) {
	green := RenderMarkdown(State{})
	require.Contains(t, green, "The upstream test suite passes on the unpatched revision")

	preExisting := RenderMarkdown(State{BaselineTestFailures: []string{"github.com/itchyny/gojq/cli::TestCliRun"}})
	require.Contains(t, preExisting, "already fails on the unpatched revision")
	require.Contains(t, preExisting, "`github.com/itchyny/gojq/cli::TestCliRun`")
	require.NotContains(t, preExisting, "passes on the unpatched revision")
}

func TestCandidateEventSummaryNamesPrimaryMetricAndReason(t *testing.T) {
	record := CandidateRecord{
		Attempt:  3,
		Decision: domain.DecisionRejected,
		Comparisons: []domain.MetricComparison{
			{Metric: "cpu_time_ns", DeltaPercent: -4.5},
			{Metric: manifest.DefaultPrimaryMetric, DeltaPercent: -1.254},
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
		Comparisons: []domain.MetricComparison{{Metric: "peak_memory_bytes", DeltaPercent: 0.5, StatisticallyFit: true}},
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

// A campaign directory outlives the build that wrote it, so a report recorded
// before comparisons carried structured workload identities has to say why its
// rows are unlabelled instead of leaving an operator to guess.
func TestRenderMarkdownExplainsUnlabelledRows(t *testing.T) {
	legacy := RenderMarkdown(State{CandidateRecords: []CandidateRecord{{
		Attempt:     1,
		Comparisons: []domain.MetricComparison{{Baseline: 100, Candidate: 90, DeltaPercent: -10}},
	}}})
	require.Contains(t, legacy, "unlabelled")
	require.Contains(t, legacy, "before comparisons carried structured workload identities")

	current := RenderMarkdown(State{CandidateRecords: []CandidateRecord{{
		Attempt:     1,
		Comparisons: []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "flatten-users", Baseline: 100, Candidate: 90, DeltaPercent: -10}},
	}}})
	require.NotContains(t, current, "unlabelled", "a current report must not carry the legacy notice")
}

// The baseline table names workloads too. It was the third surface still
// printing the derived run identifier, which the live verification caught
// after the comparison and sample tables had been fixed.
func TestRenderMarkdownLabelsBaselineWorkloads(t *testing.T) {
	report := RenderMarkdown(State{Runs: []domain.RunResult{
		{ID: "run-1", WorkloadID: "5995c3425253fee0f8a7d340", Workload: "flatten-users", Duration: time.Second},
		{ID: "run-2", WorkloadID: "6aacd1da6417eb05f09fefb9", Duration: time.Second},
	}})
	require.Contains(t, report, "`flatten-users`")
	require.NotContains(t, report, "5995c3425253fee0f8a7d340", "a report must not print a derived run identifier as a workload")
	require.Contains(t, report, "`"+unlabelledWorkload+"`", "a run recorded before labels must say so")
}

// TestRenderMarkdownNamesTheDiscoveryProfileSource: a lean campaign's
// targets came from CPU evidence plus a benchmark alloc_space profile
// (ADR 0024), and the report must say so; a campaign with no recorded source
// carries no such line.
func TestRenderMarkdownNamesTheDiscoveryProfileSource(t *testing.T) {
	report := RenderMarkdown(State{DiscoveryProfileSource: "a target sample + a benchmark alloc_space profile"})
	require.Contains(t, report, "- Discovery profile: targets chosen from a target sample + a benchmark alloc_space profile")

	require.NotContains(t, RenderMarkdown(State{}), "Discovery profile", "no recorded source means no line")
}

// A campaign in which an agent node failed must say so in its report: the
// candidate that degradation left empty is otherwise explained only as
// "patch is empty", which names the symptom and not the cause.
func TestRenderMarkdownListsDegradedRoles(t *testing.T) {
	report := RenderMarkdown(State{DegradedRoles: []RoleDegradation{{Role: "optimizer", Cause: "model stream stalled: no chunk for 2m0s"}}})
	require.Contains(t, report, "## Degraded roles")
	require.Contains(t, report, "`optimizer`: model stream stalled: no chunk for 2m0s")

	require.NotContains(t, RenderMarkdown(State{}), "Degraded roles", "a clean campaign must not carry the section")
}

// A written report carries the shape it was written in, so a reader can tell an
// old artifact from a broken one. The previous structural change was detectable
// only by classifying existing directories by hand.
func TestWriteReportsStampsTheSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	// The artifact is read back through the manifest's duration type, which
	// rejects a non-positive value, so the fixture needs the positive durations
	// a real campaign always has. That coupling between an operator artifact and
	// an engine invariant is the reason the report would rather be its own view.
	state := State{ID: "campaign-1", SchemaVersion: 0, Manifest: manifest.Manifest{Campaign: manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}}}
	require.NoError(t, WriteReports(dir, state))

	data, err := os.ReadFile(filepath.Join(dir, ReportJSONName))
	require.NoError(t, err)
	var written State
	require.NoError(t, json.Unmarshal(data, &written))
	require.Equal(t, ReportSchemaVersion, written.SchemaVersion, "the artifact must carry its shape")

	markdown, err := os.ReadFile(filepath.Join(dir, ReportMarkdownName))
	require.NoError(t, err)
	require.Contains(t, string(markdown), "- Report schema: `1`")
	require.NotContains(t, string(markdown), "unversioned")
}

func TestRenderMarkdownMarksAnUnversionedReport(t *testing.T) {
	legacy := RenderMarkdown(State{ID: "campaign-old"})
	require.Contains(t, legacy, "Report schema: unversioned")
	require.Contains(t, legacy, "written before reports carried a schema version")
}
