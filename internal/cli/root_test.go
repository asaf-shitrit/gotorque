package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/campaign"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/version"
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

func TestResumeRejectsHistory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"optimize", "--resume", t.TempDir(), "--history", t.TempDir()})

	err := cmd.Execute()
	require.ErrorContains(t, err, "--history")
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
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live", Analyst: campaign.AnalystJev})

	require.ErrorContains(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil), "pass --adk or --adk-stub")
}

func TestAttachResumeADKRejectsRunWithoutManifest(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADK: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test"})

	require.ErrorContains(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil), "--manifest is required")
}

func TestAttachResumeADKStubUsesProvidedRoles(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADKStub: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live", Analyst: campaign.AnalystJev})
	roleSet, err := agents.NewDeterministicSet()
	require.NoError(t, err)

	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, f, &roleSet, nil))
}

func TestAttachResumeADKStubBuildsRolesWhenAbsent(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir", runADKStub: true}
	engine := resumeEngine(t, campaign.State{ID: "campaign-test"})

	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, f, nil, nil))
}

func TestVerifyReportsACampaignWithNothingAccepted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"verify", writeCampaignDir(t, campaign.State{ID: "c", Status: campaign.StatusCompleted})})
	err := cmd.Execute()
	require.ErrorIs(t, err, campaign.ErrNothingAccepted)
}

// TestVerifyAcceptsAJSONFlag: the flag is bound on the command, and a campaign
// with nothing accepted still reports that instead of an empty document. The
// output path itself is covered by TestPrintVerifications..., which needs no
// campaign that measures anything.
func TestVerifyAcceptsAJSONFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"verify", "--json", writeCampaignDir(t, campaign.State{ID: "c", Status: campaign.StatusCompleted})})
	require.ErrorIs(t, cmd.Execute(), campaign.ErrNothingAccepted)
	require.Empty(t, stdout.String(), "a JSON run prints no verdict line")
}

// TestWriteVerificationPrintsWhatDecidedIt: `gotorque verify` prints the
// policy's reasons, the per-metric deltas with significance and the output
// mismatches under the verdict line, not only whether the acceptance held.
func TestWriteVerificationPrintsWhatDecidedIt(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeVerification(&out, campaign.Verification{
		Attempt: 4, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionRejected,
		Reasons:      []string{`guardrail "peak_memory_bytes" regressed by 5.58%, over the 2.00% limit`},
		Comparisons:  []domain.MetricComparison{{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546090.67, DeltaPercent: 5.58, StatisticallyFit: true, Significant: true}},
		OutputChecks: []string{"s/empty", "s/half"}, OutputMismatches: []string{"s/empty"},
	}))
	got := out.String()
	require.True(t, strings.HasPrefix(got, "attempt 4: rejected on verification over 60 pairs (originally accepted); held: false\n"), got)
	require.Contains(t, got, `- Policy: guardrail "peak_memory_bytes" regressed by 5.58%, over the 2.00% limit`)
	require.Contains(t, got, "| `peak_memory_bytes` | 3.651e+07 | 3.855e+07 | +5.58% | yes | yes |")
	require.Contains(t, got, "- Output mismatches: s/empty")
}

// stubVerifier answers Verify from a fixed map, so the verify output path is
// exercised without a campaign that measures anything.
type stubVerifier struct {
	verifications map[int]campaign.Verification
	err           error
}

func (s stubVerifier) Verify(_ context.Context, attempt, _ int) (campaign.Verification, error) {
	if s.err != nil {
		return campaign.Verification{}, s.err
	}
	return s.verifications[attempt], nil
}

// TestPrintVerificationsTextAndJSON: the text path prints one verdict block per
// attempt, with what decided it; --json emits the verifications and nothing
// else, as `report --json` emits its state.
func TestPrintVerificationsTextAndJSON(t *testing.T) {
	engine := stubVerifier{verifications: map[int]campaign.Verification{
		1: {Attempt: 1, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted},
		2: {Attempt: 2, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionRejected, Reasons: []string{"peak memory regressed 5.58%"}},
	}}

	var text bytes.Buffer
	require.NoError(t, printVerifications(context.Background(), &text, engine, []int{1, 2}, 60, false))
	require.Contains(t, text.String(), "attempt 1: accepted on verification over 60 pairs (originally accepted); held: true")
	require.Equal(t, 2, strings.Count(text.String(), "attempt "), "one verdict block per attempt")
	require.Contains(t, text.String(), "- Policy: peak memory regressed 5.58%")

	var asJSON bytes.Buffer
	require.NoError(t, printVerifications(context.Background(), &asJSON, engine, []int{1, 2}, 60, true))
	require.NotContains(t, asJSON.String(), "attempt 1: accepted", "a JSON run prints no verdict block")
	var decoded []campaign.Verification
	require.NoError(t, json.Unmarshal(asJSON.Bytes(), &decoded))
	require.Len(t, decoded, 2)
	require.Equal(t, []string{"peak memory regressed 5.58%"}, decoded[1].Reasons)

	errVerify := errors.New("verify failed")
	require.ErrorIs(t, printVerifications(context.Background(), io.Discard, stubVerifier{err: errVerify}, []int{1}, 60, false), errVerify)
}

func TestNullCandidatesRejectAgents(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"optimize", "--repo", t.TempDir(), "--manifest", "m.json", "--adk-stub", "--null-candidates", "3"})
	require.ErrorContains(t, cmd.Execute(), "--null-candidates replaces the agents")
}
