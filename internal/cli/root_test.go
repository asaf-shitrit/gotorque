package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/version"
	"github.com/stretchr/testify/require"
)

func TestVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"version", "--json"})

	require.NoError(t, cmd.Execute())
	var got version.Info
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	require.Equal(t, version.Current(), got)
	require.Empty(t, stderr.String())
}

func TestUnknownCommandDoesNotPrintUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"missing"})

	require.Error(t, cmd.Execute())
	require.Empty(t, stdout.String())
	require.Empty(t, stderr.String())
}

func TestManifestValidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"manifest", "validate", filepath.Join("..", "..", "targets", "gojq", "manifest.json")})

	require.NoError(t, cmd.Execute())
	require.Contains(t, stdout.String(), "valid target manifest")
	require.Empty(t, stderr.String())
}

// A resumed campaign reads its manifest from persisted state, and --resume
// rejects an explicit --manifest, so requiring one would make resuming a
// model-driven campaign impossible to express.
func TestResumeDoesNotRequireManifest(t *testing.T) {
	_, _, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{resume: "/tmp/campaign", runADK: true})
	require.NoError(t, err)
}

func TestFreshADKRunStillRequiresManifest(t *testing.T) {
	_, _, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{runADK: true})
	require.ErrorContains(t, err, "--manifest is required")
}

// TestOrchestratorConfigFromManifestDoesNotBindDeadlineToCommandFloor guards
// against DeterministicTimeout (the ADK scheduler's per-node deadline,
// covering evaluate_candidate: build + go test + A/B measurement + PGO)
// collapsing to minimum_command_timeout (a per-command floor, 30s in every
// shipped manifest). Before this fix that binding meant every deterministic
// node had 30s to finish, which a real evaluation blows through easily.
func TestOrchestratorConfigFromManifestDoesNotBindDeadlineToCommandFloor(t *testing.T) {
	m, err := manifest.LoadFile(filepath.Join("..", "..", "targets", "gojq", "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, m.Campaign.MinimumCommandTimeout.Duration(), "test assumes the shipped default; update the assertion below if this changes")

	config := orchestratorConfigFromManifest(m)

	require.NotEqual(t, m.Campaign.MinimumCommandTimeout.Duration(), config.DeterministicTimeout)
	require.Equal(t, orchestrator.DefaultConfig().DeterministicTimeout, config.DeterministicTimeout)
	require.Equal(t, m.Campaign.MaxCandidatePatches, config.MaxCandidates)
	require.Equal(t, m.Campaign.StopAfterFailures, config.MaxConsecutiveFailures)
}

// writeCampaignDir persists minimal campaign state into a throwaway directory
// so the resume path can open a real campaign.Engine without a Git repository.
// The persisted manifest must carry positive durations: bolt round-trips it
// through manifest.Duration, which rejects "0s" on load.
func writeCampaignDir(t *testing.T, state campaign.State) string {
	t.Helper()
	loaded, err := manifest.LoadFile(filepath.Join("..", "..", "targets", "gojq", "manifest.json"))
	require.NoError(t, err)
	state.Manifest = loaded
	dir := t.TempDir()
	store, err := campaign.OpenStore(filepath.Join(dir, campaign.DatabaseName))
	require.NoError(t, err)
	require.NoError(t, store.Save(state))
	require.NoError(t, store.Close())
	return dir
}

func resumeEngine(t *testing.T, state campaign.State) *campaign.Engine {
	t.Helper()
	engine, err := campaign.Resume(writeCampaignDir(t, state), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func TestResumeRejectsConflictingFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"optimize", "--resume", t.TempDir(), "--repo", "/tmp/repo"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "--resume cannot be combined")
}

func TestResumeReportsUnknownCampaign(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"optimize", "--resume", t.TempDir()})

	require.ErrorContains(t, cmd.Execute(), "campaign state not found")
}

// A completed campaign resumes to a no-op run, so this exercises the full
// resume path (state load, ADK attach, engine run, completion print) without
// a model endpoint or a repository.
func TestResumeCompletedCampaign(t *testing.T) {
	dir := writeCampaignDir(t, campaign.State{ID: "campaign-test", Status: campaign.StatusCompleted})
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"optimize", "--resume", dir})

	require.NoError(t, cmd.Execute())
	require.Contains(t, stdout.String(), "campaign campaign-test complete")
	require.Empty(t, stderr.String())
}

func TestAttachResumeADKRequiresFlagForModelCampaign(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir"}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live"})

	require.ErrorContains(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil), "pass --adk or --adk-stub")
}

func TestAttachResumeADKRejectsRunWithoutManifest(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADK: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test"})

	require.ErrorContains(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil), "--manifest is required")
}

func TestAttachResumeADKStubUsesProvidedRoles(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADKStub: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live"})
	roleSet, err := agents.NewDeterministicSet()
	require.NoError(t, err)

	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, f, &roleSet, nil))
}

func TestAttachResumeADKStubBuildsRolesWhenAbsent(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADKStub: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test"})

	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil))
}
