package campaign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// TestADirtiedCheckoutLeavesTheBuiltDiffUnchanged: a function_source diff is
// spliced into the target's file and diffed against it, so it applies only to
// the source it was built from. Built from the canonical checkout, it was
// built from whatever that checkout held: miller's tests truncated tracked
// fixtures there, and a diff built after that would apply to nothing but the
// dirtied tree. The same proposal must give the same diff before and after the
// canonical checkout is dirtied, and after the evaluator's own base tree is.
func TestADirtiedCheckoutLeavesTheBuiltDiffUnchanged(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	const newSrc = "func main() {\n\tb, _ := os.ReadFile(\"fixture.txt\")\n\tw := bufio.NewWriter(os.Stdout)\n\tdefer w.Flush()\n\tfmt.Fprintf(w, \"%s\", b)\n}"
	builtDiff := func(attempt int) string {
		evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
			Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
			Attempt:  attempt,
			Target:   target,
			Proposal: agents.OptimizerResult{Hypothesis: "buffer stdout", FunctionSource: newSrc, Imports: []string{"bufio"}},
		})
		require.NoError(t, err)
		require.Equal(t, FunctionSourceTransport, evidence.Candidate.Transport)
		require.NotContains(t, evidence.Summary, "rejected before build", "attempt %d: %s", attempt, evidence.FailureDetail)
		data, err := os.ReadFile(evidence.Candidate.PatchPath)
		require.NoError(t, err)
		return string(data)
	}

	clean := builtDiff(1)
	require.Contains(t, clean, "bufio.NewWriter")

	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), append([]byte("// left behind by a test run\n// and another line\n"), original...), 0o600))
	require.Equal(t, clean, builtDiff(2), "the diff was built from the dirtied canonical checkout")

	// A base tree left behind by an interrupted run, with a tracked file
	// rewritten and an untracked one added, is created again rather than read.
	baseTree := filepath.Join(engine.dir, "worktrees", "base-tree")
	require.NoDirExists(t, baseTree, "an evaluation removes the base tree it created")
	_, err = engine.newEvaluator(engine.campaignSettings()).baseRoot(context.Background())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(baseTree, "main.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(baseTree, "untracked.txt"), []byte("x"), 0o600))
	require.Equal(t, clean, builtDiff(3), "the diff was built from a dirtied base tree")

	require.NoDirExists(t, baseTree, "the evaluation removed the base tree again")
	require.NotContains(t, git(t, repo, "worktree", "list"), "base-tree", "no worktree registration outlives the evaluation")
}
