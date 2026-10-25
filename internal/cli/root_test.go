package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
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

// verifyFixtureManifest is a target manifest for the verify fixture: one tiny
// Go program, one workload, no discovery, one-minute bounds.
const verifyFixtureManifest = `{
  "version": "v1", "name": "fixture",
  "target": {"repository": "local", "build": {"package": ".", "binary": "fixture"}, "command": []},
  "workloads": {
    "seeds": [{"id": "fixture", "name": "fixture", "tier": "representative", "args": [], "files": [{"path": "fixture.txt", "content": "hello\n"}], "provenance": "test"}],
    "discovery": {"enabled": false, "sources": [], "strategies": [], "seed": 1, "max_cases": 1, "max_depth": 1},
    "tiers": {"representative": {"weight": 1.0, "acceptance_eligible": true}, "plausible": {"weight": 0.5, "acceptance_eligible": false}, "stress": {"weight": 0.0, "acceptance_eligible": false}}
  },
  "sandbox": {"network": "deny", "filesystem": {"read": "repo_and_assets", "write": "temp_only"}, "environment": {"allow": [], "passthrough": []}, "max_processes": 1},
  "normalization": {"stdout": {"mode": "exact"}, "stderr": {"mode": "exact"}, "files": []},
  "performance": {"primary_metric": "wall_time_ns", "minimum_improvement_percent": 3, "maximum_guardrail_regression_percent": 2, "statistical_support_required": true, "guardrails": []},
  "campaign": {"max_duration": "1m", "max_candidate_patches": 1, "max_concurrent_candidates": 1, "stop_after_failures": 1, "discovery_stall_timeout": "30s", "per_command_timeout_multiple": 2, "minimum_command_timeout": "10s"},
  "optimization_policy": "idiomatic"
}`

// verifyFixturePatch buffers the fixture's stdout writes: a patch the guardrails
// can accept, so a verification runs end to end.
const verifyFixturePatch = `--- a/main.go
+++ b/main.go
@@ -1,11 +1,14 @@
 package main
 
 import (
+	"bufio"
 	"fmt"
 	"os"
 )
 
 func main() {
 	b, _ := os.ReadFile("fixture.txt")
-	fmt.Printf("%s", b)
+	w := bufio.NewWriter(os.Stdout)
+	defer w.Flush()
+	_, _ = fmt.Fprintf(w, "%s", b)
 }
`

// TestVerifyJSONStdoutCarriesOnlyTheVerifications: the resumed engine reports
// each event on its progress writer, so in JSON mode that writer must not be
// stdout. It was, and `verify --json` emitted progress lines in front of the
// array, which `jq` and json.Unmarshal rejected.
func TestVerifyJSONStdoutCarriesOnlyTheVerifications(t *testing.T) {
	repo, manifestPath := writeVerifyFixture(t)
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := campaign.Create(context.Background(), campaign.Options{
		Repository: repo, ManifestPath: manifestPath, CampaignDir: campaignDir,
		TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	recordAcceptedAttempt(t, campaignDir)

	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"verify", campaignDir, "--json", "--attempt", "1", "--pairs", "2"})
	require.NoError(t, cmd.Execute())

	require.Contains(t, stderr.String(), "[candidate_verified]", "the engine's progress belongs on stderr when --json owns stdout")
	var decoded []campaign.Verification
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded), "stdout holds the array alone; it was %q", stdout.String())
	require.Len(t, decoded, 1)
	require.Equal(t, 1, decoded[0].Attempt)
}

// TestVerifyTextKeepsProgressWithTheReport: without --json the progress lines
// and the verdict blocks share the one stream an operator reads.
func TestVerifyTextKeepsProgressWithTheReport(t *testing.T) {
	repo, manifestPath := writeVerifyFixture(t)
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := campaign.Create(context.Background(), campaign.Options{
		Repository: repo, ManifestPath: manifestPath, CampaignDir: campaignDir,
		TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	recordAcceptedAttempt(t, campaignDir)

	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"verify", campaignDir, "--attempt", "1", "--pairs", "2"})
	require.NoError(t, cmd.Execute())

	require.Contains(t, stdout.String(), "attempt 1: ")
	require.Contains(t, stdout.String(), "[candidate_verified]")
	require.Empty(t, stderr.String())
}

// writeVerifyFixture builds the small Git repository and manifest a verification
// can run against, and returns their paths.
func writeVerifyFixture(t *testing.T) (repo, manifestPath string) {
	t.Helper()
	repo = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	main := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\tb, _ := os.ReadFile(\"fixture.txt\")\n\tfmt.Printf(\"%s\", b)\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(main), 0o600))
	for _, args := range [][]string{{"init"}, {"add", "main.go", "go.mod"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, string(output))
	}
	manifestPath = filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(manifestPath, []byte(verifyFixtureManifest), 0o600))
	return repo, manifestPath
}

// recordAcceptedAttempt records attempt 1 as accepted, so verify has a candidate
// to evaluate again without running a campaign that measures one.
func recordAcceptedAttempt(t *testing.T, campaignDir string) {
	t.Helper()
	patchPath := filepath.Join(t.TempDir(), "attempt-1.patch")
	require.NoError(t, os.WriteFile(patchPath, []byte(verifyFixturePatch), 0o600))
	store, err := campaign.OpenStore(filepath.Join(campaignDir, campaign.DatabaseName))
	require.NoError(t, err)
	state, err := store.Load()
	require.NoError(t, err)
	state.CandidateRecords = append(state.CandidateRecords, campaign.CandidateRecord{
		Attempt: 1, Accepted: true, Decision: domain.DecisionAccepted, PatchPath: patchPath,
		Target: &agents.Target{Location: "main.go:10", Function: "main", Cause: "unbuffered_io"},
	})
	require.NoError(t, store.Save(state))
	require.NoError(t, store.Close())
}
