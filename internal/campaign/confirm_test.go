package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/policy"
	"github.com/stretchr/testify/require"
)

// measuredEngine builds the fixture target, copies its binary as the
// candidate, and measures the two once, the way a candidate's first series is
// measured.
func measuredEngine(t *testing.T) (*Engine, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	engine, err := Create(context.Background(), Options{Repository: makeRepository(t), ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	require.NoError(t, engine.Run(context.Background()))
	binary, err := os.ReadFile(engine.state.BinaryPath)
	require.NoError(t, err)
	candidate := filepath.Join(t.TempDir(), "candidate")
	require.NoError(t, os.WriteFile(candidate, binary, 0o700)) //nolint:gosec // the candidate binary must be owner-executable; 0700 is the tightest mode that allows exec
	evidence := &orchestrator.CandidateEvidence{}
	m := &measurement{}
	require.True(t, engine.measureSeedWorkloads(context.Background(), evidence, "candidate", candidate, m), evidence.Summary)
	engine.finalizeCandidateEvidence(context.Background(), evidence, m)
	require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	return engine, evidence, m, candidate
}

// TestAnUnresolvedRegressionIsMeasuredAgain: a reading over the limit without
// significance extends every workload by a second series, and every
// comparison is derived again from both.
func TestAnUnresolvedRegressionIsMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}

	require.True(t, engine.confirmRegressions(context.Background(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples, 1)
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Len(t, evidence.RepSamples[0].CandidateNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "measured over 50 A/B pairs each; fixture +4.00% over the 2.00% limit without significance after 25 pairs, so every workload was measured over 25 more")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 104.0, c.Candidate, "the planted reading is derived again from the runs")
	}
	events, err := engine.store.Events()
	require.NoError(t, err)
	require.Equal(t, "measurement_confirmed", events[len(events)-1].Type)
}

func TestASettledVerdictIsNotMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	for _, planted := range [][]domain.MetricComparison{
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 101.5}},                  // within the limit
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 104, Significant: true}}, // a regression the policy rejects
		{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 104}},                     // not an eligible reading
	} {
		evidence.Comparisons = planted
		require.True(t, engine.confirmRegressions(context.Background(), evidence, "candidate", candidate, m))
		require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	}
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestAConfirmationSeriesIsHeldToBehaviour: the first series already marked the
// behaviour verified, so a second series whose output differs must take that
// back, or the policy would judge a changed program on its timings.
func TestAConfirmationSeriesIsHeldToBehaviour(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	require.True(t, evidence.BehaviorMatches)
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Baseline: 100, Candidate: 104}}

	require.False(t, engine.confirmRegressions(context.Background(), evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
	result := policy.Evaluate(policyConfigFromManifest(engine.state.Manifest), policy.Evidence{BehaviorMatches: evidence.BehaviorMatches, FailureSummary: evidence.Summary, SafetyChecksPassed: evidence.SafetyChecksPassed, RepresentativeEvidence: evidence.RepresentativeEvidence, Comparisons: evidence.Comparisons})
	require.Equal(t, domain.DecisionRejected, result.Decision)
}

func TestConfirmationNoteNamesEveryReading(t *testing.T) {
	note := confirmationNote([]domain.MetricComparison{{Workload: "small-doc", DeltaPercent: 4.01}, {DeltaPercent: 3.05}}, 2)
	require.True(t, strings.HasPrefix(note, "small-doc +4.01%, pooled +3.05% over the 2.00% limit"), note)
}
