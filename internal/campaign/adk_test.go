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

func TestPromoteCandidateWritesPatchAndMarksRecordAccepted(t *testing.T) {
	e := pgoLaneTestEngine(t)
	patchPath := filepath.Join(t.TempDir(), "cand.diff")
	patchBody := []byte("diff --git a/x b/x\n")
	if err := os.WriteFile(patchPath, patchBody, 0o600); err != nil {
		t.Fatal(err)
	}
	e.state.CandidateRecords = []CandidateRecord{
		{CandidateID: "other"},
		{CandidateID: "cand-1"},
	}

	err := adkServices{engine: e}.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-1", PatchPath: patchPath})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(e.dir, "accepted", "cand-1.diff"))
	require.NoError(t, err)
	require.Equal(t, patchBody, got)

	require.False(t, e.state.CandidateRecords[0].Accepted, "unrelated record must stay unaccepted")
	require.True(t, e.state.CandidateRecords[1].Accepted)

	events, err := e.store.Events()
	require.NoError(t, err)
	found := false
	for _, ev := range events {
		if ev.Type == "candidate_accepted" {
			found = true
		}
	}
	require.True(t, found, "expected candidate_accepted event, got %+v", events)
}

func TestPromoteCandidateWithEmptyPatchPathStillMarksAccepted(t *testing.T) {
	e := pgoLaneTestEngine(t)
	e.state.CandidateRecords = []CandidateRecord{{CandidateID: "cand-2"}}

	err := adkServices{engine: e}.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-2"})
	require.NoError(t, err)
	require.True(t, e.state.CandidateRecords[0].Accepted)

	entries, err := os.ReadDir(filepath.Join(e.dir, "accepted"))
	require.NoError(t, err)
	require.Empty(t, entries, "no patch means no .diff artifact")
}

func TestPromoteCandidateReportsDirectoryCreationFailure(t *testing.T) {
	// A regular file where the campaign directory should be makes
	// MkdirAll(campaign/accepted) fail before any state is touched.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{dir: blocker}
	e.state.CandidateRecords = []CandidateRecord{{CandidateID: "cand-3"}}

	err := adkServices{engine: e}.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-3", PatchPath: blocker})
	require.Error(t, err)
	require.False(t, e.state.CandidateRecords[0].Accepted)
}

func TestPromoteCandidateReportsMissingPatchFile(t *testing.T) {
	e := pgoLaneTestEngine(t)
	e.state.CandidateRecords = []CandidateRecord{{CandidateID: "cand-4"}}

	err := adkServices{engine: e}.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-4", PatchPath: filepath.Join(e.dir, "absent.diff")})
	require.Error(t, err)
	require.False(t, e.state.CandidateRecords[0].Accepted)
}

func TestPromoteCandidateReportsAcceptedArtifactWriteFailure(t *testing.T) {
	e := pgoLaneTestEngine(t)
	patchPath := filepath.Join(e.dir, "cand.diff")
	if err := os.WriteFile(patchPath, []byte("patch"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The destination path already exists as a directory, so writing the
	// accepted artifact fails after the accepted directory is created.
	acceptedDir := filepath.Join(e.dir, "accepted")
	require.NoError(t, os.MkdirAll(filepath.Join(acceptedDir, "cand-5.diff"), 0o700))

	err := adkServices{engine: e}.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-5", PatchPath: patchPath})
	require.Error(t, err)
}

// Discovery no longer carries explorer proposals: the explorer is code and
// Jev, and its workloads are sampled by the engine, so the evidence names no
// proposal counts.
func TestDiscoverCarriesNoProposalMetadata(t *testing.T) {
	engine := &Engine{state: State{}}
	evidence, err := adkServices{engine: engine}.Discover(context.Background(), orchestrator.DiscoveryRequest{})
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
func TestEvaluateAcceptsAPerWorkloadWinAndNamesIt(t *testing.T) {
	support := true
	engine := pgoLaneTestEngine(t)
	engine.state.Manifest.Performance = manifest.PerformancePolicy{
		PrimaryMetric:                     "wall_time_ns",
		MinimumImprovementPercent:         3,
		MaximumGuardrailRegressionPercent: 2,
		StatisticalSupportRequired:        &support,
		Guardrails:                        []manifest.Guardrail{{Name: "cpu_time_ns", MaximumRegressionPercent: 2, Required: true}},
	}
	services := adkServices{engine: engine}

	evaluation, err := services.Evaluate(context.Background(), orchestrator.PolicyInput{
		Evidence: orchestrator.CandidateEvidence{
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
	})
	require.NoError(t, err)
	require.Equal(t, domain.DecisionAccepted, evaluation.Decision, "reasons: %v", evaluation.Reasons)
	require.Contains(t, evaluation.Reasons[0], `workload "flatten-users"`)
	require.NotContains(t, evaluation.Reasons[0], "/wall_time_ns", "a verdict must not print the derived run identifier")
}

// An operator reading a live report after an accept should see which patch
// landed: the verdict snapshot is written before promotion, so promotion has to
// refresh it rather than leave the accepted marker for the end of the campaign.
func TestPromoteCandidateRefreshesTheLiveSnapshot(t *testing.T) {
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
	engine.state.CandidateRecords = []CandidateRecord{{Attempt: 1, CandidateID: "cand-1", Decision: domain.DecisionAccepted}}
	services := adkServices{engine: engine}

	require.NoError(t, services.PromoteCandidate(context.Background(), domain.Candidate{ID: "cand-1", PatchPath: patchPath}))

	accepted, err := os.ReadFile(filepath.Join(dir, "accepted", "cand-1.diff"))
	require.NoError(t, err)
	require.Contains(t, string(accepted), "--- a/x.go")
	snapshot, err := LoadReport(dir)
	require.NoError(t, err)
	require.Len(t, snapshot.CandidateRecords, 1)
	require.True(t, snapshot.CandidateRecords[0].Accepted, "live snapshot must show the accepted marker")
}

// TestRecordVerdictIsTheOnePlaceAVerdictIsPersisted: the graph's decision node
// and the null-candidate loop both record through recordVerdict, so one
// candidate_evaluated event, one record carrying the policy's decision, and a
// report snapshot follow from a single call, whichever caller made it.
func TestRecordVerdictIsTheOnePlaceAVerdictIsPersisted(t *testing.T) {
	e := pgoLaneTestEngine(t)
	evidence := orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "cand-v", PatchPath: "p.diff"}, Summary: "build failed", Unmeasured: true}

	result, err := e.recordVerdict(4, evidence, nil, agents.ReviewerResult{})
	require.NoError(t, err)
	require.Equal(t, domain.DecisionRejected, result.Decision)
	require.Len(t, e.state.CandidateRecords, 1)
	require.Equal(t, 4, e.state.CandidateRecords[0].Attempt)
	require.Equal(t, result.Decision, e.state.CandidateRecords[0].Decision)
	events, err := e.store.Events()
	require.NoError(t, err)
	require.Equal(t, "candidate_evaluated", events[len(events)-1].Type)
	require.FileExists(t, filepath.Join(e.dir, ReportJSONName))

	viaGraph, err := adkServices{engine: e}.Evaluate(context.Background(), orchestrator.PolicyInput{Evidence: evidence})
	require.NoError(t, err)
	require.Equal(t, result.Decision, viaGraph.Decision)
	require.Len(t, e.state.CandidateRecords, 2)
	require.Equal(t, 2, e.state.CandidateRecords[1].Attempt)
}
