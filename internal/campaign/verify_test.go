package campaign

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// TestVerifyReevaluatesARecordedCandidate: an accepted candidate is evaluated
// again from its recorded patch, with the requested pair count, and the
// verification is persisted and reported. The duplicate refusal, which would
// otherwise stop a patch this campaign already measured, is off.
func TestVerifyReevaluatesARecordedCandidate(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Target:   target,
		Proposal: agents.OptimizerResult{Hypothesis: "buffer stdout", FunctionSource: "func main() {\n\tb, _ := os.ReadFile(\"fixture.txt\")\n\tw := bufio.NewWriter(os.Stdout)\n\tdefer w.Flush()\n\tfmt.Fprintf(w, \"%s\", b)\n}", Imports: []string{"bufio"}},
	})
	require.NoError(t, err)
	engine.state.CandidateRecords = append(engine.state.CandidateRecords, CandidateRecord{
		Attempt: 1, CandidateID: evidence.Candidate.ID, Target: target, PatchPath: evidence.Candidate.PatchPath,
		Decision: domain.DecisionAccepted, Accepted: true, Comparisons: evidence.Comparisons,
	})
	require.Equal(t, []int{1}, engine.AcceptedAttempts())

	v, err := engine.Verify(context.Background(), 1, 2)
	require.NoError(t, err)
	require.Equal(t, 1, v.Attempt)
	require.Equal(t, 2, v.Pairs)
	require.Equal(t, domain.DecisionAccepted, v.Original)
	require.NotEmpty(t, v.Decision)
	require.NotContains(t, v.Summary, "identical patch was already measured")
	require.Len(t, engine.State().Verifications, 1)

	var b strings.Builder
	writeVerifications(&b, engine.State())
	require.Contains(t, b.String(), "## Verification")
	require.Contains(t, b.String(), "| 1 | 2 | accepted |")
}

func TestVerifyRefusesAnUnknownOrPatchlessAttempt(t *testing.T) {
	e := &Engine{state: State{CandidateRecords: []CandidateRecord{{Attempt: 2}}}}
	_, err := e.Verify(context.Background(), 7, 2)
	require.ErrorContains(t, err, "no attempt 7")
	_, err = e.Verify(context.Background(), 2, 2)
	require.ErrorContains(t, err, "no recorded patch")
	require.Empty(t, e.AcceptedAttempts())
}

func TestVerificationConfirmed(t *testing.T) {
	require.True(t, Verification{Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted}.Confirmed())
	require.False(t, Verification{Original: domain.DecisionAccepted, Decision: domain.DecisionInconclusive}.Confirmed())
	var b strings.Builder
	writeVerifications(&b, State{})
	require.Empty(t, b.String())
	writeVerifications(&b, State{Verifications: []Verification{{Attempt: 3, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionInconclusive, LoadAverages: []float64{9.5}, LoadContended: true}}})
	require.Contains(t, b.String(), "| 3 | 60 | accepted | inconclusive | 9.50 (contended) | 0 checked | no |")
}

func TestInputVariants(t *testing.T) {
	stdinSeed := manifest.SeedWorkload{ID: "s", StdinHeader: "h\n", Stdin: "a\nb\n", StdinRepeat: 2}
	v := inputVariants(stdinSeed)
	require.Empty(t, v["empty"].StdinBytes())
	require.Equal(t, "h\na\n", string(v["half"].StdinBytes()), "the first 5 of 10 bytes, cut back to a line boundary")
	require.Equal(t, strings.Repeat("h\na\nb\na\nb\n", 2), string(v["doubled"].StdinBytes()))

	fileSeed := manifest.SeedWorkload{ID: "f", Files: []manifest.FixtureFile{{Path: "small", Content: "x"}, {Path: "big", Content: "1\n2\n3\n4\n"}}}
	fv := inputVariants(fileSeed)
	require.Equal(t, "1\n2\n", string(fv["half"].Fixtures()["big"]))
	require.Equal(t, "x", string(fv["half"].Fixtures()["small"]), "only the largest fixture varies")
	require.Nil(t, inputVariants(manifest.SeedWorkload{ID: "none"}))
	require.Equal(t, -1, largestFixture(nil))
}

// TestVerifyOutputsCatchesADifferenceOnlyAVariantShows: the baseline
// compared with itself matches on every variant, and a candidate that
// changes its output only on empty input, which the seed never gives it, is
// caught.
func TestVerifyOutputsCatchesADifferenceOnlyAVariantShows(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	require.NoError(t, engine.runBuildStep(context.Background()))

	checks, mismatches := engine.verifyOutputs(context.Background(), engine.State().BinaryPath, "same")
	require.ElementsMatch(t, []string{"fixture/doubled", "fixture/empty", "fixture/half"}, checks)
	require.Empty(t, mismatches)

	changed := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(changed, "go.mod"), []byte("module test.local/changed\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(changed, "main.go"), []byte("package main\nimport (\"fmt\"; \"os\")\nfunc main(){ b,_:=os.ReadFile(\"fixture.txt\"); if len(b)==0 { fmt.Print(\"empty\") }; fmt.Printf(\"%s\", b) }\n"), 0o600))
	binary := filepath.Join(t.TempDir(), "changed")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Dir = changed
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	_, mismatches = engine.verifyOutputs(context.Background(), binary, "changed")
	require.Equal(t, []string{"fixture/empty"}, mismatches)

	v := Verification{Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted, OutputMismatches: mismatches}
	require.False(t, v.Confirmed(), "an output mismatch means the acceptance did not hold")
}

func TestVerificationReportNamesOutputMismatches(t *testing.T) {
	var b strings.Builder
	writeVerifications(&b, State{Verifications: []Verification{{Attempt: 1, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted, OutputChecks: []string{"s/empty", "s/half"}, OutputMismatches: []string{"s/empty"}}}})
	require.Contains(t, b.String(), "| 2 checked, differ: s/empty | no |")
}
