package campaign

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/policy"

	"github.com/stretchr/testify/require"
)

func TestDiscoverValidatesExplorerProposals(t *testing.T) {
	engine := &Engine{state: State{Manifest: manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 10, MaxDepth: 3}}}}}
	valid := agents.WorkloadProposal{Name: "smoke", Arguments: []string{"."}, Tier: domain.TierPlausible, Provenance: "test", ExpectedValid: true}
	invalid := agents.WorkloadProposal{Name: "escape", Tier: domain.TierPlausible, Provenance: "test", Fixtures: []agents.ProposedFixture{{Path: "../escape"}}}

	evidence, err := adkServices{engine: engine}.Discover(context.Background(), orchestrator.DiscoveryRequest{
		Explorer: agents.ExplorerResult{Proposals: []agents.WorkloadProposal{valid, invalid}},
	})
	require.NoError(t, err)
	require.Equal(t, "1", evidence.Metadata["proposals_accepted"])
	require.Equal(t, "1", evidence.Metadata["proposals_rejected"])
	require.Contains(t, evidence.Metadata["proposal_rejections"], "escape")
	require.Contains(t, evidence.Metadata["proposal_rejections"], "escapes sandbox")
	require.Contains(t, evidence.Summary, "(1/2 explorer proposals valid)")
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

func TestDiscoverAcceptsAllValidProposalsWithoutRejectionMetadata(t *testing.T) {
	engine := &Engine{state: State{}}
	evidence, err := adkServices{engine: engine}.Discover(context.Background(), orchestrator.DiscoveryRequest{})
	require.NoError(t, err)
	require.Equal(t, "0", evidence.Metadata["proposals_accepted"])
	require.Equal(t, "0", evidence.Metadata["proposals_rejected"])
	require.NotContains(t, evidence.Metadata, "proposal_rejections")
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

func TestEligiblePrimaryNameSelectsPooledAndPerWorkloadReadings(t *testing.T) {
	require.True(t, eligiblePrimaryName("wall_time_ns", "wall_time_ns"))
	require.True(t, eligiblePrimaryName("8a7a59da4a397c0cf4b4c5b5/wall_time_ns", "wall_time_ns"))
	require.False(t, eligiblePrimaryName("cpu_time_ns", "wall_time_ns"))
	require.False(t, eligiblePrimaryName("8a7a59da4a397c0cf4b4c5b5/cpu_time_ns", "wall_time_ns"))
	require.False(t, eligiblePrimaryName("wall_time_ns_suffix", "wall_time_ns"))
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
