package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// tieredSeed is one seed of the stress fixture manifest: the same tiny input
// in every tier, so a tier's policy is the only thing that differs.
func tieredSeed(id, tier string) map[string]any {
	return map[string]any{"id": id, "name": id, "tier": tier, "args": []string{}, "files": []any{map[string]any{"path": "fixture.txt", "content": "hello\n"}}, "provenance": "test"}
}

// stressEvaluator builds the fixture target with a plausible and a stress seed
// beside its representative one, and copies the baseline binary as the
// candidate, the way measuredEvaluator does.
func stressEvaluator(t *testing.T) (*evaluator, *Engine, string) {
	t.Helper()
	manifestPath := writeManifestWithSeeds(t, t.TempDir(), []any{
		tieredSeed("fixture", "representative"),
		tieredSeed("plausible-wide", "plausible"),
		tieredSeed("stress-deep", "stress"),
	})
	engine, err := Create(context.Background(), Options{Repository: makeRepository(t), ManifestPath: manifestPath, CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	require.NoError(t, engine.Run(context.Background()))
	binary, err := os.ReadFile(engine.state.BinaryPath)
	require.NoError(t, err)
	candidate := filepath.Join(t.TempDir(), "candidate")
	require.NoError(t, os.WriteFile(candidate, binary, 0o700)) //nolint:gosec // the candidate binary must be owner-executable; 0700 is the tightest mode that allows exec
	return engine.newEvaluator(campaignDefaults()), engine, candidate
}

// stressReadings names the workloads the stress pass reported, in order.
func stressReadings(comparisons []domain.MetricComparison) []string {
	out := make([]string, 0, len(comparisons))
	for _, c := range comparisons {
		out = append(out, c.Workload+"/"+c.Metric)
	}
	return out
}

// TestTheCandidateRunsOnWorkloadsThatCannotAccept: issue #101. The manifest
// has always documented that a stress seed can veto a candidate and that a
// plausible seed adds evidence, while every measurement loop skipped both, so
// a patch that misbehaved only on a larger input was accepted on
// representative timings alone. Both tiers are now measured, and the pass
// keeps guardrail readings only: wall time on a workload that cannot carry an
// acceptance must not be able to carry one.
func TestTheCandidateRunsOnWorkloadsThatCannotAccept(t *testing.T) {
	ev, _, candidate := stressEvaluator(t)
	evidence := &orchestrator.CandidateEvidence{Summary: "patched tree built; tests passed; 1 acceptance-eligible workload(s) measured over 25 A/B pairs each"}

	require.True(t, ev.measureStressSeeds(context.Background(), campaignDefaults(), evidence, "candidate", candidate), evidence.Summary)
	require.Equal(t, []string{
		"plausible-wide/cpu_time_ns", "plausible-wide/peak_memory_bytes",
		"stress-deep/cpu_time_ns", "stress-deep/peak_memory_bytes",
	}, stressReadings(evidence.StressComparisons))
	for _, c := range evidence.StressComparisons {
		require.NotEqual(t, "wall_time_ns", c.Metric, "a workload that cannot accept must not report the primary metric")
		require.Positive(t, c.Baseline)
	}
	require.Contains(t, evidence.Summary, "the candidate also ran on 2 workload(s) the manifest keeps out of acceptance, which can veto it and cannot carry it")
}

// TestAContendedStressPassIsMeasuredAgain: the seeds that can only veto are
// measured inside the pass measureSeedsOnQuietMachine repeats, so a load burst
// that ends the pass contended throws their readings away with the rest.
// Measured outside that retry, a burst during a stress series could push a
// guardrail reading past its limit, and without statistical support that
// reading alone rejects, which is a false reject (lead review on #109).
func TestAContendedStressPassIsMeasuredAgain(t *testing.T) {
	ev, _, candidate := stressEvaluator(t)
	ev.localIsolation = true
	runs, reset := countingCandidate(t, ev, candidate)
	s := campaignDefaults()
	measure := func() *orchestrator.CandidateEvidence {
		m := &measurement{comparisons: make([]domain.MetricComparison, 0, 4)}
		evidence := &orchestrator.CandidateEvidence{LoadAverages: []float64{1}}
		require.True(t, ev.measureSeedsOnQuietMachine(context.Background(), s, evidence, "candidate", candidate, m), evidence.Summary)
		require.Len(t, m.seeds, 1, "the fixture has one acceptance-eligible seed")
		return evidence
	}

	ev.machine = &scriptedMachine{loads: [][]float64{{2}}, limit: 5}
	quietPass := measure()
	onePass := runs()
	require.Positive(t, onePass)
	require.Len(t, quietPass.StressComparisons, 4, "both seeds the manifest keeps out of acceptance were measured")

	reset()
	burst := &scriptedMachine{loads: [][]float64{{9}, {2}}, limit: 5}
	ev.machine = burst
	burstPass := measure()
	require.Equal(t, 2*onePass, runs(), "the contended pass ran again, stress seeds included")
	require.Equal(t, 1, burst.waits)
	require.Len(t, burstPass.StressComparisons, 4, "only the second pass's stress readings reach the verdict")
	require.NotEmpty(t, burstPass.DiscardedLoad, "the discarded pass's load is kept beside the record")
}

// TestAStressWorkloadHoldsTheCandidateToItsBehaviour: the behavior comparison
// is the same one every other measurement is held to, so a candidate that
// changes output on an input no representative seed reaches is rejected
// instead of measured.
func TestAStressWorkloadHoldsTheCandidateToItsBehaviour(t *testing.T) {
	ev, engine, candidate := stressEvaluator(t)
	require.NoError(t, os.WriteFile(candidate, []byte("#!/bin/sh\necho changed\n"), 0o700)) //nolint:gosec // stands in for the candidate binary, so it must be executable
	evidence := orchestrator.CandidateEvidence{}

	require.False(t, ev.measureStressSeeds(context.Background(), campaignDefaults(), &evidence, "candidate", candidate))
	require.False(t, evidence.BehaviorMatches)
	require.Contains(t, evidence.Summary, `behavior mismatch (byte-exact comparison) on workload "plausible-wide"`)

	require.Equal(t, domain.DecisionRejected, engine.policyVerdict(evidence).Decision)
}

// TestAnAcceptanceEligibleTierIsMeasuredOnce: the tier policy is what decides
// where a seed is measured, not its name. Making the plausible tier eligible
// moves both of its seeds into the acceptance measurement and out of the veto
// pass, so no workload is measured twice and no eligible one is left unjudged.
func TestAnAcceptanceEligibleTierIsMeasuredOnce(t *testing.T) {
	ev, _, candidate := stressEvaluator(t)
	ev.manifest.Workloads.Tiers[domain.TierPlausible] = manifest.TierPolicy{Weight: 0.5, AcceptanceEligible: true}

	m := &measurement{comparisons: make([]domain.MetricComparison, 0, 4)}
	evidence := &orchestrator.CandidateEvidence{}
	require.True(t, ev.measureSeedWorkloads(context.Background(), campaignDefaults(), evidence, "candidate", candidate, m), evidence.Summary)
	require.Len(t, m.seeds, 2)
	ev.finalizeCandidateEvidence(context.Background(), evidence, m)
	require.Contains(t, evidence.Summary, "2 acceptance-eligible workload(s) measured")
	require.True(t, ev.measureStressSeeds(context.Background(), campaignDefaults(), evidence, "candidate", candidate), evidence.Summary)
	require.Equal(t, []string{"stress-deep/cpu_time_ns", "stress-deep/peak_memory_bytes"}, stressReadings(evidence.StressComparisons))
}

// TestTheVerdictReadsStressGuardrails: the readings the stress pass records
// reach the policy that draws the verdict, so a reading that regressed on a
// stress workload is judged even though the candidate won on its representative
// ones.
func TestTheVerdictReadsStressGuardrails(t *testing.T) {
	ev, engine, _ := stressEvaluator(t)
	evidence := orchestrator.CandidateEvidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 95, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 95, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		StressComparisons: []domain.MetricComparison{
			{Metric: "peak_memory_bytes", Workload: "stress-deep", Baseline: 100, Candidate: 240, Significant: true},
		},
	}
	result := engine.policyVerdict(evidence)
	require.Equal(t, domain.DecisionRejected, result.Decision)
	require.Contains(t, strings.Join(result.Reasons, " "), `on workload "stress-deep"`)

	evidence.StressComparisons[0].Candidate = 101
	require.Equal(t, domain.DecisionAccepted, engine.policyVerdict(evidence).Decision)
	require.NotEmpty(t, ev.manifest.Workloads.Seeds)
}

// stressSeedNamed is one seed of the fixture manifest by id.
func stressSeedNamed(t *testing.T, ev *evaluator, id string) manifest.SeedWorkload {
	t.Helper()
	for _, seed := range ev.manifest.Workloads.Seeds {
		if seed.ID == id {
			return seed
		}
	}
	t.Fatalf("the fixture manifest has no seed %q", id)
	return manifest.SeedWorkload{}
}

// TestAStressMemoryBreachIsMeasuredAgainBeforeItVetoes: peak memory is a
// per-run high-water mark on a seed that can only veto as well, so such a
// breach needs a fresh series before it may reject (ADR 0040, ADR 0046). The
// engine measures that series on the breaching seed alone, records its own
// reading under the seed id, and the verdict lets the candidate through when
// the fresh series reads the breach away.
func TestAStressMemoryBreachIsMeasuredAgainBeforeItVetoes(t *testing.T) {
	ev, engine, candidate := stressEvaluator(t)
	runs, reset := countingCandidate(t, ev, candidate)
	seed := stressSeedNamed(t, ev, "stress-deep")
	breach := []domain.MetricComparison{{Metric: "peak_memory_bytes", Workload: seed.ID, Baseline: 100, Candidate: 140, Significant: true}}

	evidence := &orchestrator.CandidateEvidence{}
	reset()
	require.True(t, ev.replicateStressBreach(context.Background(), campaignDefaults(), seed, "candidate", candidate, evidence, &seedRuns{seed: seed, deterministic: true}, breach), evidence.Summary)
	require.Positive(t, runs(), "the seed whose peak memory breached was measured again")
	require.Len(t, evidence.FreshGuardrails, 1, "the fresh series' own reading is recorded")
	require.Equal(t, "stress-deep", evidence.FreshGuardrails[0].Workload)
	require.Equal(t, "peak_memory_bytes", evidence.FreshGuardrails[0].Metric)
	require.Contains(t, evidence.Summary, `workload "stress-deep"'s peak_memory_bytes read +40.00% over its limit`)

	verdict := engine.policyVerdict(orchestrator.CandidateEvidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 95, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 95, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		StressComparisons: breach,
		FreshGuardrails:   evidence.FreshGuardrails,
	})
	require.Equal(t, domain.DecisionAccepted, verdict.Decision, verdict.Reasons)
	require.Contains(t, strings.Join(verdict.Reasons, " "), "did not reproduce the breach")
}

// TestOnlyAStressMemoryBreachAsksForAFreshSeries: a reading inside its limit
// costs no series, because no reading on such a workload can carry an
// acceptance and a real regression is caught by the limit itself.
func TestOnlyAStressMemoryBreachAsksForAFreshSeries(t *testing.T) {
	ev, _, candidate := stressEvaluator(t)
	runs, reset := countingCandidate(t, ev, candidate)
	seed := stressSeedNamed(t, ev, "stress-deep")
	underLimit := []domain.MetricComparison{{Metric: "peak_memory_bytes", Workload: seed.ID, Baseline: 100, Candidate: 101, Significant: true}}

	evidence := &orchestrator.CandidateEvidence{}
	reset()
	require.True(t, ev.replicateStressBreach(context.Background(), campaignDefaults(), seed, "candidate", candidate, evidence, &seedRuns{seed: seed, deterministic: true}, underLimit))
	require.Zero(t, runs(), "a reading inside its limit is not measured again")
	require.Empty(t, evidence.FreshGuardrails)
	require.Empty(t, evidence.Summary)
}

// TestAConfirmationSeriesKeepsTheStressFreshReadings: the stress replication
// reading is keyed by workload, and a confirmation series replaces only the
// pooled readings. Overwriting the whole field would leave the policy with no
// fresh reading for a stress breach, which is the single-series rule ADR 0040
// removed and the false veto the stress pass must not produce.
func TestAConfirmationSeriesKeepsTheStressFreshReadings(t *testing.T) {
	ev, _, _ := stressEvaluator(t)
	stress := domain.MetricComparison{Metric: "peak_memory_bytes", Workload: "stress-deep", Baseline: 100, Candidate: 140, Significant: true}
	evidence := &orchestrator.CandidateEvidence{FreshGuardrails: []domain.MetricComparison{stress}}
	m := &measurement{seeds: []seedRuns{{seed: stressSeedNamed(t, ev, "fixture"), deterministic: true}}}

	ev.recordGuardrailSeries(evidence, m, nil, []int{0})

	require.Contains(t, evidence.FreshGuardrails, stress, "the stress seed's fresh reading survived the confirmation series")
	for _, comparison := range evidence.FreshGuardrails {
		if comparison.Metric == "peak_memory_bytes" && comparison.Workload == "" {
			return // the pooled reading was recorded beside it
		}
	}
	t.Fatalf("the pooled reading was not recorded: %+v", evidence.FreshGuardrails)
}

// TestTheReportShowsStressReadings: a veto names a workload in one sentence, so
// the reading behind it has to be in the report; otherwise nobody can audit it.
func TestTheReportShowsStressReadings(t *testing.T) {
	var b strings.Builder
	writeCandidateComparisons(&b, CandidateRecord{StressComparisons: []domain.MetricComparison{
		{Metric: "peak_memory_bytes", Workload: "stress-deep", Baseline: 100, Candidate: 140, DeltaPercent: 40, Significant: true},
		{Metric: "cpu_time_ns", Workload: "stress-deep", Baseline: 100, Candidate: 101, DeltaPercent: 1},
	}})
	require.Contains(t, b.String(), "Guardrails on the workloads the manifest keeps out of acceptance")
	require.Contains(t, b.String(), "| `stress-deep` | `peak_memory_bytes` | 100 | 140 | +40.00% | yes |")
	require.Contains(t, b.String(), "| `stress-deep` | `cpu_time_ns` | 100 | 101 | +1.00% | no |")
}
