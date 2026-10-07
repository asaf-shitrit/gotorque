package campaign

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/policy"

	"github.com/stretchr/testify/require"
)

// TestCampaignRequestCarriesTheObjective: the CampaignRequest built for every
// ADK node, including the analyst's CauseRequest.Campaign, carries the
// manifest's resolved primary metric, so a campaign started with
// --tradeoff lean reaches causeAnalyst.AnalyzeCauses as "peak_memory_bytes"
// and a manifest left at its default reaches it as "wall_time_ns" (ADR 0024).
func TestCampaignRequestCarriesTheObjective(t *testing.T) {
	lean := &Engine{state: State{Manifest: manifest.Manifest{Performance: manifest.PerformancePolicy{PrimaryMetric: "peak_memory_bytes"}}}}
	require.Equal(t, "peak_memory_bytes", lean.campaignRequest().Objective)

	balanced := &Engine{state: State{Manifest: manifest.Manifest{Performance: manifest.PerformancePolicy{PrimaryMetric: "wall_time_ns"}}}}
	require.Equal(t, "wall_time_ns", balanced.campaignRequest().Objective)
}

// acceptedSettlement is a settlement of a candidate whose verdict is accepted.
// Settle records the verdict it is handed, so the tests state it directly.
func acceptedSettlement(candidate domain.Candidate) orchestrator.Settlement {
	return orchestrator.Settlement{
		Assessment: orchestrator.Assessment{
			Evidence: orchestrator.CandidateEvidence{Candidate: candidate, Summary: "measured"},
			Verdict:  domain.Evaluation{CandidateID: candidate.ID, Decision: domain.DecisionAccepted},
		},
		Progress: orchestrator.CampaignProgress{CandidatesTried: 1, LastDecision: domain.DecisionAccepted, CandidateID: candidate.ID},
	}
}

func eventKinds(t *testing.T, e *Engine) []string {
	t.Helper()
	events, err := e.store.Events()
	require.NoError(t, err)
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Type)
	}
	return kinds
}

func TestSettleWritesPatchAndRecordsTheCandidateAccepted(t *testing.T) {
	e := pgoLaneTestEngine(t)
	patchPath := filepath.Join(t.TempDir(), "cand.diff")
	patchBody := []byte("diff --git a/x b/x\n")
	if err := os.WriteFile(patchPath, patchBody, 0o600); err != nil {
		t.Fatal(err)
	}
	e.state.CandidateRecords = []CandidateRecord{{CandidateID: "other"}}

	require.NoError(t, engineBench{engine: e}.Settle(context.Background(), acceptedSettlement(domain.Candidate{ID: "cand-1", PatchPath: patchPath})))

	got, err := os.ReadFile(filepath.Join(e.dir, "accepted", "cand-1.diff"))
	require.NoError(t, err)
	require.Equal(t, patchBody, got)
	require.Len(t, e.state.CandidateRecords, 2)
	require.False(t, e.state.CandidateRecords[0].Accepted, "unrelated record must stay unaccepted")
	require.Equal(t, "cand-1", e.state.CandidateRecords[1].CandidateID)
	require.Equal(t, 2, e.state.CandidateRecords[1].Attempt)
	require.True(t, e.state.CandidateRecords[1].Accepted)
}

// TestSettleOrdersWhatItMakesDurable pins the sequence an interrupted campaign
// can be resumed from: the artifact is written before the record that refers to
// it, the record is saved with its accepted marker in one state, and the
// tallies come last, so no stop bound ever counts a verdict that is not on disk
// and no record exists without its promotion.
func TestSettleOrdersWhatItMakesDurable(t *testing.T) {
	e := pgoLaneTestEngine(t)
	patchPath := filepath.Join(t.TempDir(), "cand.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte("patch"), 0o600))
	settlement := acceptedSettlement(domain.Candidate{ID: "cand-1", PatchPath: patchPath})
	settlement.Progress = orchestrator.CampaignProgress{CandidatesTried: 1, ConsecutiveFailures: 0, LastDecision: domain.DecisionAccepted, CandidateID: "cand-1"}
	e.state.ConsecutiveFailures = 3
	// The saved state is read back through the manifest's duration type, which
	// rejects a non-positive value.
	e.state.Manifest.Campaign = manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}

	require.NoError(t, engineBench{engine: e}.Settle(context.Background(), settlement))

	require.Equal(t, []string{"candidate_evaluated", "candidate_accepted", "adk_progress"}, eventKinds(t, e))
	reloaded, err := e.store.Load()
	require.NoError(t, err)
	require.Len(t, reloaded.CandidateRecords, 1)
	require.True(t, reloaded.CandidateRecords[0].Accepted, "the saved record carries its promotion")
	require.Zero(t, reloaded.ConsecutiveFailures, "the tallies are saved with the progress event")
	require.FileExists(t, filepath.Join(e.dir, "accepted", "cand-1.diff"))
}

// TestSettleLeavesNoRecordWhenPromotionFails: a promotion that cannot happen
// fails the settlement before the verdict is recorded, so a resumed campaign
// evaluates the attempt again instead of finding an accepted verdict whose
// patch never landed.
func TestSettleLeavesNoRecordWhenPromotionFails(t *testing.T) {
	missing := func(e *Engine) domain.Candidate {
		return domain.Candidate{ID: "cand-4", PatchPath: filepath.Join(e.dir, "absent.diff")}
	}
	blocked := func(e *Engine) domain.Candidate {
		patchPath := filepath.Join(e.dir, "cand.diff")
		require.NoError(t, os.WriteFile(patchPath, []byte("patch"), 0o600))
		// The destination already exists as a directory, so writing the
		// accepted artifact fails after the accepted directory is created.
		require.NoError(t, os.MkdirAll(filepath.Join(e.dir, "accepted", "cand-5.diff"), 0o700))
		return domain.Candidate{ID: "cand-5", PatchPath: patchPath}
	}
	for name, candidateOf := range map[string]func(*Engine) domain.Candidate{"patch file is missing": missing, "artifact cannot be written": blocked} {
		t.Run(name, func(t *testing.T) {
			e := pgoLaneTestEngine(t)
			err := engineBench{engine: e}.Settle(context.Background(), acceptedSettlement(candidateOf(e)))
			require.Error(t, err)
			require.Empty(t, e.state.CandidateRecords)
			require.Empty(t, eventKinds(t, e), "nothing is recorded, so nothing is counted")
		})
	}
}

func TestSettleReportsDirectoryCreationFailure(t *testing.T) {
	// A regular file where the campaign directory should be makes
	// MkdirAll(campaign/accepted) fail before any state is touched.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{dir: blocker}

	err := engineBench{engine: e}.Settle(context.Background(), acceptedSettlement(domain.Candidate{ID: "cand-3", PatchPath: blocker}))
	require.Error(t, err)
	require.Empty(t, e.state.CandidateRecords)
}

func TestSettleWithEmptyPatchPathStillRecordsTheCandidateAccepted(t *testing.T) {
	e := pgoLaneTestEngine(t)

	require.NoError(t, engineBench{engine: e}.Settle(context.Background(), acceptedSettlement(domain.Candidate{ID: "cand-2"})))
	require.True(t, e.state.CandidateRecords[0].Accepted)

	entries, err := os.ReadDir(filepath.Join(e.dir, "accepted"))
	require.NoError(t, err)
	require.Empty(t, entries, "no patch means no .diff artifact")
}

// TestSettleRecordsTheVerdictItIsGiven: Settle does not judge. A rejected
// verdict is recorded as rejected whatever the evidence beside it says, which
// is what makes computing the verdict before the reviewer ran safe.
func TestSettleRecordsTheVerdictItIsGiven(t *testing.T) {
	e := pgoLaneTestEngine(t)
	settlement := acceptedSettlement(domain.Candidate{ID: "cand-6"})
	settlement.Assessment.Verdict = domain.Evaluation{CandidateID: "cand-6", Decision: domain.DecisionRejected, Reasons: []string{"decided upstream"}}
	settlement.Progress = orchestrator.CampaignProgress{CandidatesTried: 1, ConsecutiveFailures: 1, LastDecision: domain.DecisionRejected, CandidateID: "cand-6"}

	require.NoError(t, engineBench{engine: e}.Settle(context.Background(), settlement))

	require.Equal(t, domain.DecisionRejected, e.state.CandidateRecords[0].Decision)
	require.Equal(t, []string{"decided upstream"}, e.state.CandidateRecords[0].Reasons)
	require.False(t, e.state.CandidateRecords[0].Accepted)
	require.Equal(t, []string{"candidate_evaluated", "adk_progress"}, eventKinds(t, e))
	require.Equal(t, 1, e.state.ConsecutiveFailures)
}

// Discovery no longer carries explorer proposals: the explorer is code and
// Jev, and its workloads are sampled by the engine, so the evidence names no
// proposal counts.
func TestDiscoverCarriesNoProposalMetadata(t *testing.T) {
	engine := &Engine{state: State{}}
	evidence, err := engineBench{engine: engine}.Discovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, "baseline discovery evidence", evidence.Summary)
	require.NotContains(t, evidence.Metadata, "proposals_accepted")
	require.NotContains(t, evidence.Metadata, "proposals_rejected")
	require.NotContains(t, evidence.Metadata, "entry_points")
}

// The manifest's performance block is the acceptance contract between a
// repository and this harness. It used to be loaded, defaulted, and validated,
// and then ignored: the policy was handed policy.DefaultConfig(), so a target
// asking for a 5% floor or a narrower guardrail list was judged by 3% and the
// default guardrails.
func TestPolicyConfigComesFromTheManifest(t *testing.T) {
	support := false
	m := manifest.Manifest{Performance: manifest.PerformancePolicy{
		PrimaryMetric:                     "peak_memory_bytes",
		MinimumImprovementPercent:         5,
		MaximumGuardrailRegressionPercent: 1.5,
		StatisticalSupportRequired:        &support,
		Guardrails:                        []manifest.Guardrail{{Name: "cpu_time_ns", MaximumRegressionPercent: 0.5, Required: true}},
	}}
	config := policyConfigFromManifest(m)
	require.Equal(t, "peak_memory_bytes", config.PrimaryMetric)
	require.InDelta(t, 5.0, config.MinimumImprovementPercent, 1e-9)
	require.InDelta(t, 1.5, config.MaximumGuardrailRegressionPercent, 1e-9)
	require.False(t, config.StatisticalSupportRequired)
	require.Len(t, config.Guardrails, 1)
	require.Equal(t, "cpu_time_ns", config.Guardrails[0].Name)
	require.InDelta(t, 0.5, config.Guardrails[0].MaximumRegressionPercent, 1e-9)
	require.True(t, config.Guardrails[0].Required)
}

// An empty performance block must fall back to the documented defaults rather
// than judging every candidate by a zero threshold.
func TestPolicyConfigFallsBackToDefaults(t *testing.T) {
	config := policyConfigFromManifest(manifest.Manifest{})
	require.Equal(t, policy.DefaultConfig(), config)
}

// Eligibility is structural now, so the seam is where it gets tested: the
// campaign hands the policy every reading of the primary metric, a supported
// win on one eligible workload carries acceptance even when the pooled figure
// is below the threshold, and the reason names that workload by its manifest
// seed id rather than the derived run identifier it used to print.
func TestJudgeAcceptsAPerWorkloadWinAndNamesIt(t *testing.T) {
	support := true
	engine := pgoLaneTestEngine(t)
	engine.state.Manifest.Performance = manifest.PerformancePolicy{
		PrimaryMetric:                     "wall_time_ns",
		MinimumImprovementPercent:         3,
		MaximumGuardrailRegressionPercent: 2,
		StatisticalSupportRequired:        &support,
		Guardrails:                        []manifest.Guardrail{{Name: "cpu_time_ns", MaximumRegressionPercent: 2, Required: true}},
	}

	evaluation := engine.judge(
		orchestrator.CandidateEvidence{
			Candidate:              domain.Candidate{ID: "candidate-1"},
			BehaviorMatches:        true,
			SafetyChecksPassed:     true,
			RepresentativeEvidence: true,
			Comparisons: []domain.MetricComparison{
				// The pool over both representative workloads is only 2% better,
				// which the threshold refuses on its own.
				{Metric: "wall_time_ns", Unit: "ns", Baseline: 1_000_000, Candidate: 980_000, StatisticallyFit: true},
				// One eligible workload improved 5% with support.
				{Metric: "wall_time_ns", Workload: "flatten-users", Unit: "ns", Baseline: 600_000, Candidate: 570_000, StatisticallyFit: true},
				{Metric: "cpu_time_ns", Unit: "ns", Baseline: 500_000, Candidate: 500_000, StatisticallyFit: true},
			},
		},
	)
	require.Equal(t, domain.DecisionAccepted, evaluation.Decision, "reasons: %v", evaluation.Reasons)
	require.Contains(t, evaluation.Reasons[0], `workload "flatten-users"`)
	require.NotContains(t, evaluation.Reasons[0], "/wall_time_ns", "a verdict must not print the derived run identifier")
}

// An operator reading a live report after an accept should see which patch
// landed: the record is saved with its accepted marker, so the one snapshot
// taken with the verdict already shows it rather than leaving the marker for the
// end of the campaign.
func TestSettleWritesTheLiveSnapshotWithTheAcceptedMarker(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, err)
	patchPath := filepath.Join(dir, "candidate.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte("--- a/x.go\n+++ b/x.go\n"), 0o600))
	engine := &Engine{dir: dir, store: store, progress: io.Discard, now: func() time.Time { return time.Now().UTC() }}
	// The snapshot is read back through the manifest's duration type, which
	// rejects a non-positive value, so the fixture needs the positive durations
	// the loader guarantees in a real campaign.
	engine.state.Manifest.Campaign = manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}

	require.NoError(t, engineBench{engine: engine}.Settle(context.Background(), acceptedSettlement(domain.Candidate{ID: "cand-1", PatchPath: patchPath})))

	accepted, err := os.ReadFile(filepath.Join(dir, "accepted", "cand-1.diff"))
	require.NoError(t, err)
	require.Contains(t, string(accepted), "--- a/x.go")
	snapshot, err := LoadReport(dir)
	require.NoError(t, err)
	require.Len(t, snapshot.CandidateRecords, 1)
	require.True(t, snapshot.CandidateRecords[0].Accepted, "live snapshot must show the accepted marker")
}

// settleJudged judges evidence as Assess would and settles it, the path every
// graph candidate takes once it has been evaluated.
func settleJudged(t *testing.T, e *Engine, evidence orchestrator.CandidateEvidence, target *agents.Target, review agents.ReviewerResult) domain.Evaluation {
	t.Helper()
	verdict := e.judge(evidence)
	require.NoError(t, engineBench{engine: e}.Settle(context.Background(), orchestrator.Settlement{
		Assessment: orchestrator.Assessment{Evidence: evidence, Verdict: verdict},
		Target:     target,
		Review:     review,
		Progress:   orchestrator.CampaignProgress{CandidatesTried: len(e.state.CandidateRecords) + 1, LastDecision: verdict.Decision, CandidateID: verdict.CandidateID},
	}))
	return verdict
}

// TestEveryVerdictIsPersistedByPersistVerdict: the graph's settlement and the
// null-candidate loop both record through persistVerdict, so one
// candidate_evaluated event, one record carrying the policy's decision, and a
// report snapshot follow from a single call, whichever caller made it.
func TestEveryVerdictIsPersistedByPersistVerdict(t *testing.T) {
	e := pgoLaneTestEngine(t)
	evidence := orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "cand-v", PatchPath: "p.diff"}, Summary: "build failed", Unmeasured: true}

	result, err := e.recordVerdict(4, evidence, nil, agents.ReviewerResult{})
	require.NoError(t, err)
	require.Equal(t, domain.DecisionRejected, result.Decision)
	require.Len(t, e.state.CandidateRecords, 1)
	require.Equal(t, 4, e.state.CandidateRecords[0].Attempt)
	require.Equal(t, result.Decision, e.state.CandidateRecords[0].Decision)
	require.Equal(t, "candidate_evaluated", eventKinds(t, e)[0])
	require.FileExists(t, filepath.Join(e.dir, ReportJSONName))

	viaGraph := settleJudged(t, e, evidence, nil, agents.ReviewerResult{})
	require.Equal(t, result.Decision, viaGraph.Decision)
	require.Len(t, e.state.CandidateRecords, 2)
	require.Equal(t, 2, e.state.CandidateRecords[1].Attempt)
}
