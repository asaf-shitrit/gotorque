package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNullCandidatesRunThroughTheWholeCampaign: a campaign in null mode
// evaluates the requested number of comment-only candidates through the real
// evaluation and policy, accepts none, and finishes with a null stop reason.
func TestNullCandidatesRunThroughTheWholeCampaign(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true, NullCandidates: 2,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	require.NoError(t, engine.Run(context.Background()))

	state := engine.State()
	require.Equal(t, StatusCompleted, state.Status)
	require.Equal(t, "2 null candidates evaluated", state.StopReason)
	require.Len(t, state.CandidateRecords, 2)
	for _, r := range state.CandidateRecords {
		require.False(t, r.Accepted, "a comment-only candidate must never be accepted: %+v", r)
		require.Contains(t, r.Hypothesis, "null candidate")
		require.NotEmpty(t, r.Comparisons, "the null candidate reached measurement: %s / %s", r.Summary, r.FailureDetail)
	}
	require.NotEqual(t, state.CandidateRecords[0].CandidateID, state.CandidateRecords[1].CandidateID)
}

func TestNullPatchAddsACommentAfterThePackageClause(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte("// Doc.\n\npackage a\n\nfunc f() {}\n"), 0o600))
	patch, err := nullPatch(repo, "a.go", 7)
	require.NoError(t, err)
	require.Equal(t, "--- a/a.go\n+++ b/a.go\n@@ -3,1 +3,2 @@\n package a\n+// gotorque null candidate 7\n", patch)

	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte("// no clause\n"), 0o600))
	_, err = nullPatch(repo, "b.go", 1)
	require.ErrorContains(t, err, "no package clause")
	_, err = nullPatch(repo, "missing.go", 1)
	require.Error(t, err)
}

func TestNullTargetFiles(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cmd", "tool"), 0o700))
	for _, name := range []string{"b.go", "a.go", "a_test.go", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, "cmd", "tool", name), []byte("package main\n"), 0o600))
	}
	files, err := nullTargetFiles(repo, "", "./cmd/tool")
	require.NoError(t, err)
	require.Equal(t, []string{"cmd/tool/a.go", "cmd/tool/b.go"}, files)

	require.NoError(t, os.MkdirAll(filepath.Join(repo, "empty"), 0o700))
	_, err = nullTargetFiles(repo, "", "empty")
	require.ErrorContains(t, err, "no Go file")
	_, err = nullTargetFiles(repo, "", "absent")
	require.True(t, err != nil && strings.Contains(err.Error(), "list the build package"))
}

// TestNullCandidatesStopWhenTheContextEnds is null-gron's tail: once
// max_duration cancels the campaign context, no further attempt is recorded
// as a rejection; the run returns the context's error so the campaign stops
// as interrupted.
func TestNullCandidatesStopWhenTheContextEnds(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true, NullCandidates: 3,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = engine.runNullCandidates(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, engine.State().CandidateRecords)
}
