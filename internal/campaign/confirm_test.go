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

// measuredEvaluator builds the fixture target, copies its binary as the
// candidate, and measures the two once, the way a candidate's first series is
// measured.
func measuredEvaluator(t *testing.T) (*evaluator, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	return measuredEvaluatorWith(t, campaignDefaults())
}

// measuredEvaluatorWith is measuredEvaluator with the settings a caller other than a
// campaign would pass, such as verification's pair count.
func measuredEvaluatorWith(t *testing.T, s evalSettings) (*evaluator, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	_, ev, evidence, m, candidate := measuredEngineEvaluator(t, s)
	return ev, evidence, m, candidate
}

// measuredTwoWorkloadEvaluator measures the fixture target, which carries two
// representative workloads, so a verdict has more than one reading to choose
// the best from.
func measuredTwoWorkloadEvaluator(t *testing.T, s evalSettings) (*evaluator, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	_, ev, evidence, m, candidate := measuredOverManifest(t, s, twoWorkloadManifest)
	return ev, evidence, m, candidate
}

// twoWorkloadManifest writes the fixture target's manifest with two
// representative workloads.
func twoWorkloadManifest(t *testing.T, dir string) string {
	t.Helper()
	return writeManifestWithSeeds(t, dir, []any{fixtureSeed("fixture", []string{}), fixtureSeed("second", []string{"second"})})
}

// measuredEngineEvaluator keeps the engine, for a test that drives the
// evaluator's measurement and then the engine's verdict path over the evidence
// it produced.
func measuredEngineEvaluator(t *testing.T, s evalSettings) (*Engine, *evaluator, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	return measuredOverManifest(t, s, writeManifest)
}

// measuredOverManifest is measuredEngineEvaluator with the manifest chosen by
// the caller.
func measuredOverManifest(t *testing.T, s evalSettings, manifestPath func(t *testing.T, dir string) string) (*Engine, *evaluator, *orchestrator.CandidateEvidence, *measurement, string) {
	t.Helper()
	engine, err := Create(context.Background(), Options{Repository: makeRepository(t), ManifestPath: manifestPath(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	require.NoError(t, engine.Run(context.Background()))
	binary, err := os.ReadFile(engine.state.BinaryPath)
	require.NoError(t, err)
	candidate := filepath.Join(t.TempDir(), "candidate")
	require.NoError(t, os.WriteFile(candidate, binary, 0o700)) //nolint:gosec // the candidate binary must be owner-executable; 0700 is the tightest mode that allows exec
	ev := engine.newEvaluator(s)
	evidence := &orchestrator.CandidateEvidence{}
	m := &measurement{}
	require.True(t, ev.measureSeedWorkloads(context.Background(), s, evidence, "candidate", candidate, m), evidence.Summary)
	ev.finalizeCandidateEvidence(context.Background(), evidence, m)
	require.Len(t, evidence.RepSamples[0].BaselineNs, s.pairs)
	return engine, ev, evidence, m, candidate
}

// TestAnUnresolvedRegressionIsMeasuredAgain: a reading over the limit without
// significance extends every workload by a second series, and every
// comparison is derived again from both.
func TestAnUnresolvedRegressionIsMeasuredAgain(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}

	journal := &recordingJournal{}
	ev.journal = journal
	require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples, 1)
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Len(t, evidence.RepSamples[0].CandidateNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "measured over 50 A/B pairs each; fixture +4.00% over the 2.00% limit without significance after 25 pairs, so every workload was measured over 25 more")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 104.0, c.Candidate, "the planted reading is derived again from the runs")
	}
	require.Equal(t, "measurement_confirmed", journal.events[len(journal.events)-1].kind)
}

func TestASettledVerdictIsNotMeasuredAgain(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	for _, planted := range [][]domain.MetricComparison{
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 101.5}},                   // within the limit
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 104, Significant: true}},  // a regression the policy rejects
		{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 104, Significant: true}},   // a guardrail the policy rejects
		{{Metric: "cpu_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}, // a per-workload guardrail reading, not the pooled one
	} {
		evidence.Comparisons = planted
		require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
		require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	}
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestAConfirmationSeriesIsHeldToBehaviour: the first series already marked the
// behaviour verified, so a second series whose output differs must take that
// back, or the policy would judge a changed program on its timings.
func TestAConfirmationSeriesIsHeldToBehaviour(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	require.True(t, evidence.BehaviorMatches)
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Baseline: 100, Candidate: 104}}

	require.False(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
	result := policy.Evaluate(policyConfigFromManifest(ev.manifest), policy.Evidence{BehaviorMatches: evidence.BehaviorMatches, FailureSummary: evidence.Summary, SafetyChecksPassed: evidence.SafetyChecksPassed, RepresentativeEvidence: evidence.RepresentativeEvidence, Comparisons: evidence.Comparisons})
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
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 103.06}}

	require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
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
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91}, // 9% improvement, unsupported
	}, guardrailsUnaffected()...)

	journal := &recordingJournal{}
	ev.journal = journal
	require.True(t, ev.confirmImprovements(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples, 1)
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Len(t, evidence.RepSamples[0].CandidateNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "improved past the 3.00% minimum without significance after 25 pairs, so every workload was measured over 25 more")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 91.0, c.Candidate, "the planted reading is derived again from the runs")
	}
	require.Equal(t, "improvement_confirmed", journal.events[len(journal.events)-1].kind)
}

// TestASettledImprovementVerdictIsNotMeasuredAgain: a supported win, or one
// below the minimum, is not extended.
func TestASettledImprovementVerdictIsNotMeasuredAgain(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	for _, planted := range [][]domain.MetricComparison{
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 91, StatisticallyFit: true}}, // supported win
		{{Metric: "wall_time_ns", Baseline: 100, Candidate: 99}},                         // below the minimum
	} {
		evidence.Comparisons = append(append([]domain.MetricComparison{}, planted...), guardrailsUnaffected()...)
		require.True(t, ev.confirmImprovements(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
		require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	}
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestImprovementConfirmationIsHeldToBehaviour mirrors
// TestAConfirmationSeriesIsHeldToBehaviour on the improvement side: the first
// series already marked the behaviour verified, so a second series whose
// output differs must take that back.
func TestImprovementConfirmationIsHeldToBehaviour(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	require.True(t, evidence.BehaviorMatches)
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91},
	}, guardrailsUnaffected()...)

	require.False(t, ev.confirmImprovements(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
}

// TestAtMostOneExtraSeriesPerCandidate: when the regression confirmation
// already extended every seed, the improvement confirmation must not run a
// second extra series.
func TestAtMostOneExtraSeriesPerCandidate(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.ValidationJobs = append(evidence.ValidationJobs, "interleaved-ab-confirmation")
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91}, // would otherwise trigger a second series
	}, guardrailsUnaffected()...)

	require.True(t, ev.confirmImprovements(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
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
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 96.84, StatisticallyFit: true, Significant: true},
	}, guardrailsUnaffected()...)

	require.True(t, ev.confirmImprovements(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
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
	regression := func(e *evaluator, s evalSettings, ev *orchestrator.CandidateEvidence, c string, m *measurement) bool {
		return e.confirmRegressions(context.Background(), s, ev, "candidate", c, m)
	}
	improvement := func(e *evaluator, s evalSettings, ev *orchestrator.CandidateEvidence, c string, m *measurement) bool {
		return e.confirmImprovements(context.Background(), s, ev, "candidate", c, m)
	}
	for name, tc := range map[string]struct {
		planted []domain.MetricComparison
		confirm func(*evaluator, evalSettings, *orchestrator.CandidateEvidence, string, *measurement) bool
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
			ev, evidence, m, candidate := measuredEvaluatorWith(t, s)
			evidence.Comparisons = tc.planted
			require.True(t, tc.confirm(ev, s, evidence, candidate, m))
			require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs)
			require.Contains(t, evidence.Summary, tc.want)
		})
	}
}

// perWorkloadAccept plants the readings of a candidate whose verdict would
// accept on one workload's reading: the pooled primary stays below the
// minimum, so acceptance rests on the workload that read best of two.
func perWorkloadAccept() []domain.MetricComparison {
	return append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
		{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 93.49, StatisticallyFit: true, Significant: true},
		{Metric: "wall_time_ns", Workload: "second", Baseline: 100, Candidate: 99, StatisticallyFit: true},
	}, guardrailsUnaffected()...)
}

// TestAPerWorkloadAcceptIsMeasuredAgainFromScratch: ADR 0041. The accept rested
// on one workload's reading, which was chosen because it read best of several,
// so that workload alone is measured over a fresh series and judged on that
// series alone: the series that selected it is replaced, not extended.
func TestAPerWorkloadAcceptIsMeasuredAgainFromScratch(t *testing.T) {
	s := evalSettings{pairs: 5, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	evidence.Comparisons = perWorkloadAccept()

	journal := &recordingJournal{}
	ev.journal = journal
	require.True(t, ev.confirmSelection(context.Background(), s, evidence, "candidate", candidate, m))

	require.Len(t, evidence.RepSamples, 2)
	require.Len(t, evidence.RepSamples[0].BaselineNs, s.pairs, "the selecting series is replaced, not extended")
	require.Len(t, evidence.RepSamples[1].BaselineNs, s.pairs, "the workload the accept did not rest on is untouched")
	require.Contains(t, evidence.Summary, `workload "fixture" read -6.51% as the best of the workload readings, so it alone was measured over 5 fresh pairs and judged on those alone:`)
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
	require.Equal(t, "selection_confirmed", journal.events[len(journal.events)-1].kind)
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 93.49, c.Candidate, "the planted reading is derived again from the fresh series")
	}
}

// TestAPooledAcceptIsNotMeasuredAgainForSelection: an accept that rests on the
// pooled metric never chose among the workload readings, so the bias this
// corrects is not there.
func TestAPooledAcceptIsNotMeasuredAgainForSelection(t *testing.T) {
	s := evalSettings{pairs: 3, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 91, StatisticallyFit: true, Significant: true},
		{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 98, StatisticallyFit: true},
		{Metric: "wall_time_ns", Workload: "second", Baseline: 100, Candidate: 99, StatisticallyFit: true},
	}, guardrailsUnaffected()...)

	require.True(t, ev.confirmSelection(context.Background(), s, evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, s.pairs)
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestASingleWorkloadAcceptIsNotMeasuredAgainForSelection: with one
// representative workload there was no choice among readings to inflate.
func TestASingleWorkloadAcceptIsNotMeasuredAgainForSelection(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
		{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 93.49, StatisticallyFit: true, Significant: true},
	}, guardrailsUnaffected()...)

	require.True(t, ev.confirmSelection(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	require.NotContains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")
}

// TestSelectionConfirmationIsHeldToBehaviour mirrors the other confirmation
// series: a fresh series whose output differs takes the verified behaviour
// back, so the policy never judges a changed program on its timings.
func TestSelectionConfirmationIsHeldToBehaviour(t *testing.T) {
	s := evalSettings{pairs: 3, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	evidence.Comparisons = perWorkloadAccept()
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable

	require.False(t, ev.confirmSelection(context.Background(), s, evidence, "candidate", candidate, m))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "fixture"`)
}

// TestAPerWorkloadAcceptIsMeasuredAgainAfterARegressionConfirmation: a
// regression confirmation extends every seed rather than replacing it, so a
// per-workload accept then rests on the 50 pooled pairs that still hold the 25
// that selected the workload. The selected workload is measured fresh and the
// pooled series is replaced, so the accept is judged on the fresh reading alone
// (ADR 0041). The fresh series is as long as the one it replaces, because the
// pooled reading folds the seeds per repetition.
func TestAPerWorkloadAcceptIsMeasuredAgainAfterARegressionConfirmation(t *testing.T) {
	s := evalSettings{pairs: 3, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 104}}
	require.True(t, ev.confirmRegressions(context.Background(), s, evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs, "the regression confirmation extended every workload")

	evidence.Comparisons = perWorkloadAccept()
	require.True(t, ev.confirmSelection(context.Background(), s, evidence, "candidate", candidate, m))

	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs, "the fresh series replaces the extended one")
	require.Len(t, evidence.RepSamples[1].BaselineNs, 2*s.pairs, "the workload the accept did not rest on is untouched")
	require.Contains(t, evidence.Summary, `workload "fixture" read -6.51% as the best of the workload readings, so it alone was measured over 6 fresh pairs and judged on those alone:`)
	require.Equal(t, 1, countJob(evidence.ValidationJobs, "interleaved-ab-confirmation"), "the confirmation job is recorded once")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 93.49, c.Candidate, "the planted reading is derived again from the fresh series")
	}
}

// TestAPerWorkloadAcceptIsMeasuredAgainAfterAnImprovementConfirmation: an
// unsupported-improvement confirmation extends every seed, so a per-workload
// accept that follows it would rest on the pooled pairs that still hold the
// ones that selected the workload. The selected workload is measured fresh and
// the pooled series is replaced, so the accept is judged on the fresh reading
// alone (ADR 0041).
func TestAPerWorkloadAcceptIsMeasuredAgainAfterAnImprovementConfirmation(t *testing.T) {
	s := evalSettings{pairs: 3, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	// An improvement past the minimum without support: the verdict would be
	// inconclusive, so the improvement confirmation runs.
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 95},
		{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 95},
		{Metric: "wall_time_ns", Workload: "second", Baseline: 100, Candidate: 96},
	}, guardrailsUnaffected()...)
	require.True(t, ev.confirmImprovements(context.Background(), s, evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs, "the improvement confirmation extended every workload")

	evidence.Comparisons = perWorkloadAccept()
	require.True(t, ev.confirmSelection(context.Background(), s, evidence, "candidate", candidate, m))

	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs, "the fresh series replaces the extended one")
	require.Len(t, evidence.RepSamples[1].BaselineNs, 2*s.pairs, "the workload the accept did not rest on keeps both series")
	require.Contains(t, evidence.Summary, `workload "fixture" read -6.51% as the best of the workload readings, so it alone was measured over 6 fresh pairs and judged on those alone:`)
	require.Equal(t, 1, countJob(evidence.ValidationJobs, "interleaved-ab-confirmation"), "the confirmation job is recorded once")
	for _, c := range evidence.Comparisons {
		require.NotEqual(t, 93.49, c.Candidate, "the planted reading is derived again from the fresh series")
	}
}

// TestTheExtendingConfirmationRunsBeforeTheSelectionOne: order is what keeps a
// per-workload accept off a pooled series. A borderline per-workload accept
// gets the extending confirmation, which doubles every seed, and the selection
// confirmation runs after it. Run the other way round, the selection
// re-measurement spends its series first, the extending step then adds its
// pairs to the workload that selected the accept, and the verdict rests on the
// pooled reading the fresh series was meant to replace (ADR 0041).
func TestTheExtendingConfirmationRunsBeforeTheSelectionOne(t *testing.T) {
	s := evalSettings{pairs: 3, refuseRepeats: true}
	ev, evidence, m, candidate := measuredTwoWorkloadEvaluator(t, s)
	// A per-workload accept that cleared the minimum by less than a factor of
	// two: borderline, so the extending confirmation runs on it.
	evidence.Comparisons = append([]domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
		{Metric: "wall_time_ns", Workload: "fixture", Baseline: 100, Candidate: 96.9, StatisticallyFit: true, Significant: true},
		{Metric: "wall_time_ns", Workload: "second", Baseline: 100, Candidate: 99.5, StatisticallyFit: true},
	}, guardrailsUnaffected()...)
	journal := &recordingJournal{}
	ev.journal = journal

	require.True(t, ev.secondLook(context.Background(), s, evidence, "candidate", candidate, m))

	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*s.pairs, "the extending confirmation ran first")
	require.Contains(t, evidence.Summary, `fixture improved by 3.10%, less than twice the 3.00% minimum, so every workload was measured over 3 more before the verdict`)
	require.NotContains(t, evidence.Summary, "so it alone was measured", "the selection confirmation did not run on the first series")
	require.Equal(t, 1, countJob(evidence.ValidationJobs, "interleaved-ab-confirmation"), "the confirmation job is recorded once")
	require.Equal(t, "improvement_confirmed", journal.events[len(journal.events)-1].kind)
}

func countJob(jobs []string, job string) int {
	count := 0
	for _, j := range jobs {
		if j == job {
			count++
		}
	}
	return count
}

// TestASignificantPeakMemoryBreachIsMeasuredAgain: a supported peak-memory
// breach used to be final on its first series. Peak RSS is a per-run
// high-water mark that steps with allocator and GC pacing, so the same two
// binaries read +5.58% on one 60-pair verification and -0.68% on the next,
// and a candidate that only removed garbage lost a supported -3.4% wall-time
// win to a +2.78% reading it did not cause. A fresh series is measured, and
// its own readings are recorded for the policy.
func TestASignificantPeakMemoryBreachIsMeasuredAgain(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546091, StatisticallyFit: true, Significant: true}}

	journal := &recordingJournal{}
	ev.journal = journal
	require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, 2*measurementRepetitions)
	require.Contains(t, evidence.Summary, "peak_memory_bytes +5.58% over the 2.00% limit and significant after 25 pairs, so a fresh series of 25 pairs was measured to check it reproduces")
	require.Contains(t, evidence.ValidationJobs, "interleaved-ab-confirmation")

	metrics := make([]string, 0, len(evidence.FreshGuardrails))
	for _, c := range evidence.FreshGuardrails {
		metrics = append(metrics, c.Metric)
		require.Empty(t, c.Workload, "the fresh readings are pooled ones")
		require.Positive(t, c.Baseline)
	}
	require.ElementsMatch(t, []string{"cpu_time_ns", "peak_memory_bytes"}, metrics)
	require.Equal(t, "measurement_confirmed", journal.events[len(journal.events)-1].kind)
}

// TestAVerdictQuotesTheFirstSeriesReading: the reason a breach did not
// reproduce says what the first series read, and the engine records that
// reading before the fresh one is folded in. The pooled comparisons hold one
// value for both series, so once a fresh series ran they are no longer what the
// first series produced.
func TestAVerdictQuotesTheFirstSeriesReading(t *testing.T) {
	engine, ev, evidence, m, candidate := measuredEngineEvaluator(t, campaignDefaults())
	breach := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546091, StatisticallyFit: true, Significant: true}
	evidence.BehaviorMatches, evidence.SafetyChecksPassed, evidence.RepresentativeEvidence = true, true, true
	evidence.Comparisons = []domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 80, StatisticallyFit: true, Significant: true},
		breach,
		{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
	}

	require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.FirstSeriesGuardrails, 1, "the first series' peak-memory reading is recorded")
	require.InDelta(t, breach.Candidate, evidence.FirstSeriesGuardrails[0].Candidate, 0.0)

	// The fresh series reads quiet, so the breach did not reproduce and the
	// verdict names both readings instead of rejecting. The comparisons are the
	// pooled readings the confirmation derived from both series; the fixture
	// builds no binary of its own, so its binary-size reading is not usable.
	evidence.Comparisons = []domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 80, StatisticallyFit: true, Significant: true},
		{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 37497005, StatisticallyFit: true, Significant: true},
		{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
	}
	evidence.FreshGuardrails = []domain.MetricComparison{{Metric: "peak_memory_bytes", Baseline: breach.Baseline, Candidate: breach.Baseline, StatisticallyFit: true}}
	result := engine.policyVerdict(*evidence)
	require.NotEqual(t, domain.DecisionRejected, result.Decision)
	require.Contains(t, strings.Join(result.Reasons, "; "), `guardrail "peak_memory_bytes" read +5.58% on the first series, over the 2.00% limit and significant, but a fresh series read +0.00% and did not reproduce the breach`)
}

// TestASupportedCPUGuardrailBreachStaysFinal: only peak memory is replicated.
// A supported CPU-time breach rejects on its first series, as ADR 0033 says.
func TestASupportedCPUGuardrailBreachStaysFinal(t *testing.T) {
	ev, evidence, m, candidate := measuredEvaluator(t)
	evidence.Comparisons = []domain.MetricComparison{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 106, StatisticallyFit: true, Significant: true}}

	require.True(t, ev.confirmRegressions(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m))
	require.Len(t, evidence.RepSamples[0].BaselineNs, measurementRepetitions)
	require.Empty(t, evidence.FreshGuardrails)
}

// TestPolicyVerdictReadsTheFreshSeries pins the wiring: the verdict must see
// the fresh series, or a breach it did not reproduce would reject anyway. After
// a fresh series the pooled comparisons hold both series together, so the
// first series' own reading arrives on the evidence beside them.
func TestPolicyVerdictReadsTheFreshSeries(t *testing.T) {
	engine, err := Create(context.Background(), Options{
		Repository: makeRepository(t), ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	breach := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546091, StatisticallyFit: true, Significant: true}
	comparisons := []domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 80, StatisticallyFit: true},
		{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 37500000, StatisticallyFit: true, Significant: true},
		{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
	}
	evidence := orchestrator.CandidateEvidence{BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true, Comparisons: comparisons, FirstSeriesGuardrails: []domain.MetricComparison{breach}}
	require.Equal(t, domain.DecisionRejected, engine.policyVerdict(evidence).Decision, "with no fresh series the single-series rule holds")

	evidence.FreshGuardrails = []domain.MetricComparison{{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 36507648}}
	result := engine.policyVerdict(evidence)
	require.Equal(t, domain.DecisionAccepted, result.Decision)
	reasons := strings.Join(result.Reasons, "; ")
	require.Contains(t, reasons, "did not reproduce the breach")
	require.Contains(t, reasons, "read +5.58% on the first series", "the reason quotes the first series, not the pooled +2.72%")
}
