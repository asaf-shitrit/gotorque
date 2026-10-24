package campaign

import (
	"context"
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
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
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
	require.Len(t, evidence.ArtifactURIs, 1, "nothing was built")
}
