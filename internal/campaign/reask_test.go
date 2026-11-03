package campaign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

// refusingRepo is methodRepo's source, so a hand-written hunk can carry real
// surrounding context and still remove a line that is not there: the shape a
// model's diff takes when its positions drifted, which neither strict git apply
// nor the fuzzy fallback can place.
func refusingRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	src := `package main

import "os"

type cli struct{ out *os.File }

// printValues writes every value on its own line.
func (c *cli) printValues(vs []string) error {
	for _, v := range vs {
		c.out.WriteString(v)
		c.out.WriteString("\n")
	}
	return nil
}

func other() int { return 1 }
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(src), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

// TestEvaluateCandidateTellsAnApplyRefusalApart: an unmeasured rejection is one
// of several, and only a patch git refuses to apply can be answered by asking
// the optimizer for function_source instead (issue #72, graph's reaskable). The
// evidence has to name that one, because the graph routes on it and on nothing
// else.
func TestEvaluateCandidateTellsAnApplyRefusalApart(t *testing.T) {
	repo := refusingRepo(t)
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: campaignDir, TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	patch := "--- a/main.go\n+++ b/main.go\n@@ -7,7 +7,7 @@\n // printValues writes every value on its own line.\n func (c *cli) printValues(vs []string) error {\n-for _, v := range before {\n+for _, v := range after {\n \t\tc.out.WriteString(v)\n \t\tc.out.WriteString(\"\\n\")\n \t}\n \treturn nil\n"
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Proposal: agents.OptimizerResult{Patch: patch, Hypothesis: "buffer the writes"},
	})
	require.NoError(t, err)
	require.True(t, evidence.Unmeasured)
	require.Contains(t, evidence.Summary, "candidate rejected before build")
	require.Contains(t, evidence.ReaskCause, "does not apply")
	require.Contains(t, evidence.ReaskCause, "main.go")
	// The cause is quoted back to the optimizer, so it names the refusal and
	// not the campaign's own patch artifact path.
	require.NotContains(t, evidence.ReaskCause, campaignDir)
	require.Len(t, evidence.ArtifactURIs, 1, "nothing was built")
}

// unresolvedEvidence is what an evaluation reports for a proposal that could
// not become a diff, over the fixture repository.
func unresolvedEvidence(t *testing.T, target *agents.Target, role string, proposal agents.OptimizerResult) orchestrator.CandidateEvidence {
	t.Helper()
	engine, err := Create(context.Background(), Options{
		Repository: refusingRepo(t), ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign:    orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:     1,
		Target:      target,
		RoleFailure: role,
		Proposal:    proposal,
	})
	require.NoError(t, err)
	require.True(t, evidence.Unmeasured)
	return evidence
}

var printValuesTarget = agents.Target{Location: "main.go:8", Function: "(*cli).printValues", Cause: "unbuffered_io"}

// TestATruncatedFunctionSourceIsAskedAgainWithTheDeclarationsSize: issue #77's
// yaegi and csvq attempts. The answer ends inside the function, the parser says
// so at EOF, and the refusal that goes back to the optimizer names the parse
// error and how long the declaration is.
func TestATruncatedFunctionSourceIsAskedAgainWithTheDeclarationsSize(t *testing.T) {
	truncated := "func (c *cli) printValues(vs []string) error {\n\tfor _, v := range vs {\n\t\tc.out.WriteString(v)\n"
	evidence := unresolvedEvidence(t, &printValuesTarget, "", agents.OptimizerResult{Hypothesis: "h", FunctionSource: truncated})

	require.Contains(t, evidence.Summary, "does not parse as Go source")
	require.Contains(t, evidence.ReaskCause, "found 'EOF'")
	require.Contains(t, evidence.ReaskCause, "(*cli).printValues is 7 lines long at the base revision")
	require.Contains(t, evidence.ReaskCause, "every brace closed")
}

// TestAnEmptyAnswerIsAskedAgain: tomlv, chroma, shfmt and yq each returned a
// hypothesis and no code. Nothing was judged, so the attempt may ask once more.
func TestAnEmptyAnswerIsAskedAgain(t *testing.T) {
	evidence := unresolvedEvidence(t, &printValuesTarget, "", agents.OptimizerResult{Hypothesis: "compute the indent once"})
	require.Equal(t, errEmptyProposal.Error(), evidence.ReaskCause)
}

// TestRefusalsTheOptimizerCannotAnswerAreNotAskedAgain: a function_source that
// parses but names the wrong function, a source that is not Go, a model call
// that never answered and a target-less proposal each say something the same
// transport would repeat or have nothing to re-ask for.
func TestRefusalsTheOptimizerCannotAnswerAreNotAskedAgain(t *testing.T) {
	cases := []struct {
		name     string
		target   *agents.Target
		role     string
		proposal agents.OptimizerResult
	}{
		{"another function", &printValuesTarget, "", agents.OptimizerResult{FunctionSource: "func other() int { return 2 }"}},
		{"not go", &printValuesTarget, "", agents.OptimizerResult{FunctionSource: "this is prose"}},
		{"the model call failed", &printValuesTarget, "optimizer model call failed after 3 attempts", agents.OptimizerResult{}},
		{"no target", nil, "", agents.OptimizerResult{Hypothesis: "h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := unresolvedEvidence(t, tc.target, tc.role, tc.proposal)
			require.Empty(t, evidence.ReaskCause)
		})
	}
}

func TestOnlyAnEndedTooEarlySourceIsATruncation(t *testing.T) {
	for src, want := range map[string]bool{
		"func f() {":               true,
		"func f() {\n\ts := \"abc": true,
		"func f() {\n\t/* open":    true,
		"func f( {":                false,
		"func f() { x := }":        false,
		"func f() {}\nfunc g() {}": false,
		"this is prose":            false,
	} {
		_, err := splitFunctionDecls(src)
		var truncated truncationError
		require.Equal(t, want, errors.As(err, &truncated), "%q: %v", src, err)
	}
}
