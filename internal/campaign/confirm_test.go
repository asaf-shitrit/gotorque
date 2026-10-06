package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/policy"
	"github.com/stretchr/testify/require"
)

// measuredEngine builds the fixture target, copies its binary as the
// candidate, and measures the two once, the way a candidate's first series is
// measured.
func measuredEngine(t *testing.T) (*Engine, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	return measuredEngineWith(t, evalSettings{pairs: measurementRepetitions, refuseRepeats: true, pgoLane: true})
}

// measuredEngineWith is measuredEngine with the settings a caller other than a
// campaign would pass, such as verification's pair count.
func measuredEngineWith(t *testing.T, s evalSettings) (*Engine, *orchestrator.CandidateEvidence, *measurement, string) {
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
	require.True(t, engine.measureSeedWorkloads(context.Background(), s, evidence, "candidate", candidate, m), evidence.Summary)
	engine.finalizeCandidateEvidence(context.Background(), evidence, m)
	require.Len(t, evidence.RepSamples[0].BaselineNs, s.pairs)
	return engine, evidence, m, candidate
}

// TestAnUnresolvedRegressionIsMeasuredAgain: a reading over the limit without
// significance extends every workload by a second series, and every
// comparison is derived again from both.
func TestAnUnresolvedRegressionIsMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}

	require.True(t, engine.confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
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
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 101.5}},                   // within the limit
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 104, Significant: true}},  // a regression the policy rejects
		{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 104, Significant: true}},   // a guardrail the policy rejects
		{{Metric: "cpu_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}, // a per-workload guardrail reading, not the pooled one
	} {
		evidence.Comparisons = planted
		require.True(t, engine.confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
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

	require.False(t, engine.confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
	result := policy.Evaluate(policyConfigFromManifest(engine.state.Manifest), policy.Evidence{BehaviorMatches: evidence.BehaviorMatches, FailureSummary: evidence.Summary, SafetyChecksPassed: evidence.SafetyChecksPassed, RepresentativeEvidence: evidence.RepresentativeEvidence, Comparisons: evidence.Comparisons})
	require.Equal(t, domain.DecisionRejected, result.Decision)
}

func TestConfirmationNoteNamesEveryReading(t *testing.T) {
	note := confirmationNote([]domain.MetricComparison{
		{Metric: "wall_time_ns", Workload: "small-doc", DeltaPercent: 4.01},
		{Metric: "wall_time_ns", DeltaPercent: 3.05},
		{Metric: "cpu_time_ns", DeltaPercent: 2.16},
	}, 2, "wall_time_ns", measurementRepetitions)
	require.True(t, strings.HasPrefix(note, "small-doc +4.01%, pooled +3.05%, cpu_time_ns +2.16% over the 2.00% limit"), note)
}

// TestAnUnresolvedGuardrailIsMeasuredAgain: the null-gron case. A pooled
// guardrail past its limit without significance no longer rejects on its
// point estimate, so it gets the second series that lets a real regression
// become significant.
func TestAnUnresolvedGuardrailIsMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 103.06}}

	require.True(t, engine.confirmRegressions(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "cpu_time_ns +3.06% over the 2.00% limit without significance after 25 pairs")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// guardrailsUnaffected plants supported, non-regressing guardrail readings so
// a planted primary-metric comparison is the only thing an evaluation could
// turn on.
func guardrailsUnaffected() []domain.MetricComparison {
	return []domain.MetricComparison{
		{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
	}
}

// TestAnUnsupportedPromisingImprovementIsMeasuredAgain: ADR 0021. A reading
// that improved by at least the manifest's minimum but lacks statistical
// support would otherwise end inconclusive, so it is measured again and
// every comparison is derived from both series.
func TestAnUnsupportedPromisingImprovementIsMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91}, // 9% improvement, unsupported
	}, guardrailsUnaffected()...)

	require.True(t, engine.confirmImprovements(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples, 1)
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Len(t, evidence.RepSamples[0].CandidateNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "improved past the 3.00% minimum without significance after 25 pairs, so every workload was measured over 25 more")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 91.0, c.Candidate, "the planted reading is derived again from the runs")
	}
	events, err := engine.store.Events()
	require.NoError(t, err)
	require.Equal(t, "improvement_confirmed", events[len(events)-1].Type)
}

// TestASettledImprovementVerdictIsNotMeasuredAgain: a supported win, or one
// below the minimum, is not extended.
func TestASettledImprovementVerdictIsNotMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	for _, planted := range [][]domain.MetricComparison{
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 91, StatisticallyFit: true}}, // supported win
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 99}},                         // below the minimum
	} {
		evidence.Comparisons = append(append([]domain.MetricComparison{}, planted...), guardrailsUnaffected()...)
		require.True(t, engine.confirmImprovements(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
		require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	}
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestImprovementConfirmationIsHeldToBehaviour mirrors
// TestAConfirmationSeriesIsHeldToBehaviour on the improvement side: the first
// series already marked the behaviour verified, so a second series whose
// output differs must take that back.
func TestImprovementConfirmationIsHeldToBehaviour(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	require.True(t, evidence.BehaviorMatches)
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91},
	}, guardrailsUnaffected()...)

	require.False(t, engine.confirmImprovements(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
}

// TestAtMostOneExtraSeriesPerCandidate: when the regression confirmation
// already extended every seed, the improvement confirmation must not run a
// second extra series.
func TestAtMostOneExtraSeriesPerCandidate(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91}, // would otherwise trigger a second series
	}, guardrailsUnaffected()...)

	require.True(t, engine.confirmImprovements(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions, "no additional series was measured")
}

func TestImprovementConfirmationNoteNamesEveryReading(t *testing.T) {
	note := improvementConfirmationNote([]domain.MetricComparison{{Workload: "small-doc", DeltaPercent: -11.55}, {DeltaPercent: -3.05}}, 3, measurementRepetitions)
	require.True(t, strings.HasPrefix(note, "small-doc -11.55%, pooled -3.05% improved past the 3.00% minimum"), note)
}

func TestUnconfirmedImprovementsTruthTable(t *testing.T) {
	config := policy.Config{StatisticalSupportRequired: true, MinimumImprovementPercent: 3}
	for name, tc := range map[string]struct {
		comparison domain.MetricComparison
		want       int
	}{
		"unsupported and past the minimum":     {domain.MetricComparison{Baseline: 100, Candidate: 91}, 1},
		"supported and past the minimum":       {domain.MetricComparison{Baseline: 100, Candidate: 91, StatisticallyFit: true}, 0},
		"unsupported but below the minimum":    {domain.MetricComparison{Baseline: 100, Candidate: 99}, 0},
		"unsupported regression, not improved": {domain.MetricComparison{Baseline: 100, Candidate: 110}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := policy.UnconfirmedImprovements(config, []domain.MetricComparison{tc.comparison})
			require.Len(t, got, tc.want)
		})
	}
	noSupport := config
	noSupport.StatisticalSupportRequired = false
	require.Empty(t, policy.UnconfirmedImprovements(noSupport, []domain.MetricComparison{{Baseline: 100, Candidate: 91}}))
}

// TestABorderlineAcceptIsMeasuredAgain: held-out dyff was accepted on a
// supported -3.16% against the 3% minimum and verified at -2.17%. A supported
// win that clears the minimum by less than twice it now gets the second series
// before the verdict; a clear win does not (TestASettledImprovementVerdictIsNotMeasuredAgain).
func TestABorderlineAcceptIsMeasuredAgain(t *testing.T) {
	engine, evidence, m, candidate := measuredEngine(t)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 96.84, StatisticallyFit: true, Significant: true},
	}, guardrailsUnaffected()...)

	require.True(t, engine.confirmImprovements(context.Background(), engine.campaignSettings(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "pooled improved by 3.16%, less than twice the 3.00% minimum, so every workload was measured over 25 more before the verdict")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestConfirmationNotesNameTheSeriesLengthThatRan: a verification measures 60
// pairs, not a campaign's 25. The notes hard-coded the campaign's count, so a
// 60-pair verification reported "after 25 pairs ... 25 more" over a record that
// held 120 samples.
func TestConfirmationNotesNameTheSeriesLengthThatRan(t *testing.T) {
	s := evalSettings{pairs: 3}
	regression := func(e *Engine, s evalSettings, ev *orchestrator.CandidateEvidence, c string, m *measurement) bool {
		return e.confirmRegressions(context.Background(), s, ev, "candidate", c, m)
	}
	improvement := func(e *Engine, s evalSettings, ev *orchestrator.CandidateEvidence, c string, m *measurement) bool {
		return e.confirmImprovements(context.Background(), s, ev, "candidate", c, m)
	}
	for name, tc := range map[string]struct {
		planted []domain.MetricComparison
		confirm func(*Engine, evalSettings, *orchestrator.CandidateEvidence, string, *measurement) bool
		want    string
	}{
		"regression": {
			planted: []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}},
			confirm: regression,
			want:    "without significance after 3 pairs, so every workload was measured over 3 more",
		},
		"improvement": {
			planted: append([]domain.MetricComparison{{Metric: "wall_time_ns", Baseline: 100, Candidate: 91}}, guardrailsUnaffected()...),
			confirm: improvement,
			want:    "without significance after 3 pairs, so every workload was measured over 3 more",
		},
		"borderline accept": {
			planted: append([]domain.MetricComparison{{Metric: "wall_time_ns", Baseline: 100, Candidate: 96.84, StatisticallyFit: true, Significant: true}}, guardrailsUnaffected()...),
			confirm: improvement,
			want:    "so every workload was measured over 3 more before the verdict",
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine, evidence, m, candidate := measuredEngineWith(t, s)
			evidence.Comparisons = tc.planted
			require.True(t, tc.confirm(engine, s, evidence, candidate, m))
			require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs)
			require.Contains(t, evidence.Summary, tc.want)
		})
	}
}
