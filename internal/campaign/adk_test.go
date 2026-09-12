package campaign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"

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
