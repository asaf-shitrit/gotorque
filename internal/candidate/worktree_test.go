package candidate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/toolchain"
)

// preparedRepository commits a small module holding every kind of path a
// candidate must not touch next to one it may.
func preparedRepository(t *testing.T) (*WorktreeManager, string) {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		"go.mod":                "module x\n\ngo 1.26\n",
		"main.go":               "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n",
		"main_test.go":          "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n",
		"testdata/golden.txt":   "hello\n",
		"vendor/dep/dep.go":     "package dep\n",
		"vendor/modules.txt":    "# dep\n",
		"internal/tool/tool.go": "package tool\n",
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	manager := &WorktreeManager{Toolchain: toolchain.New(toolchain.Options{}), Repository: repo, Root: t.TempDir()}
	return manager, repo
}

func writePatch(t *testing.T, patch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidate.diff")
	require.NoError(t, os.WriteFile(path, []byte(patch), 0o600))
	return path
}

func TestPrepareAppliesAnOrdinarySourcePatch(t *testing.T) {
	manager, _ := preparedRepository(t)
	patch := "--- a/main.go\n+++ b/main.go\n@@ -3,3 +3,3 @@\n func main() {\n-\tprintln(\"hello\")\n+\tprintln(\"hi\")\n }\n"
	prepared, err := manager.Prepare(context.Background(), "HEAD", writePatch(t, patch), "", Policy{})
	require.NoError(t, err)
	defer func() { require.NoError(t, prepared.Close(context.Background())) }()
	data, err := os.ReadFile(filepath.Join(prepared.Worktree, "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(data), `println("hi")`)
}

// Each of these passed the old +++-only header check and reached the build.
func TestPrepareRejectsPatchesThatTouchTheGate(t *testing.T) {
	cases := map[string]struct {
		patch string
		want  string
	}{
		"deletion of go.mod": {
			"diff --git a/go.mod b/go.mod\ndeleted file mode 100644\n--- a/go.mod\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-module x\n-\n-go 1.26\n",
			`"go.mod" is off-limits`,
		},
		"rename of a vendored file": {
			"diff --git a/vendor/dep/dep.go b/dep.go\nsimilarity index 50%\nrename from vendor/dep/dep.go\nrename to dep.go\n--- a/vendor/dep/dep.go\n+++ b/dep.go\n@@ -1 +1,2 @@\n package dep\n+// moved\n",
			`"vendor/dep/dep.go" is off-limits`,
		},
		"test file edit": {
			"--- a/main_test.go\n+++ b/main_test.go\n@@ -4,1 +4,1 @@\n-func TestMain(t *testing.T) {}\n+func TestMain(t *testing.T) { t.Skip() }\n",
			`test file "main_test.go" is off-limits`,
		},
		"golden file edit": {
			"--- a/testdata/golden.txt\n+++ b/testdata/golden.txt\n@@ -1 +1 @@\n-hello\n+hi\n",
			`test file "testdata/golden.txt" is off-limits`,
		},
		"old and new names disagree": {
			"--- a/go.mod\n+++ b/other.go\n@@ -1,3 +1,3 @@\n module x\n \n-go 1.26\n+go 1.20\n",
			`"go.mod" is off-limits`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			manager, _ := preparedRepository(t)
			_, err := manager.Prepare(context.Background(), "HEAD", writePatch(t, tc.patch), "", Policy{})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// Header validation reads the diff; the patch tool decides what it edits. With
// `--- a/go.mod` and `+++ b/other.go` strict git apply refuses (other.go does
// not exist) and the fuzzy fallback rewrites go.mod. This drives that exact
// sequence without the header check, to prove the post-apply check rejects it
// on the strength of the worktree alone.
func TestPostApplyCheckCatchesTheFileTheFuzzyFallbackChose(t *testing.T) {
	manager, _ := preparedRepository(t)
	patchPath := writePatch(t, "--- a/go.mod\n+++ b/other.go\n@@ -1,3 +1,3 @@\n module x\n \n-go 1.26\n+go 1.20\n")
	worktree := filepath.Join(manager.Root, "candidate-direct")
	ctx := context.Background()
	_, err := manager.Toolchain.CreateWorktree(ctx, manager.Repository, worktree, "HEAD")
	require.NoError(t, err)
	prepared := &Prepared{Worktree: worktree, manager: manager}
	defer func() { _ = prepared.Close(ctx) }()

	_, checkErr := manager.Toolchain.ApplyPatchCheck(ctx, worktree, patchPath)
	require.Error(t, checkErr, "strict apply must refuse, or the fuzzy path is not what this exercises")
	require.NoError(t, manager.applyPreparedPatch(ctx, prepared, worktree, patchPath))
	data, err := os.ReadFile(filepath.Join(worktree, "go.mod"))
	require.NoError(t, err)
	require.Contains(t, string(data), "go 1.20", "the fallback edited the --- file, not the +++ one")

	err = rejectProtectedChanges(ctx, manager.Toolchain, worktree)
	require.Error(t, err)
	require.Contains(t, err.Error(), `applied patch changed a protected path: dependency or PGO file "go.mod" is off-limits`)
}

func TestPostApplyCheckRefusesSymlinksAndPassesCleanTrees(t *testing.T) {
	manager, _ := preparedRepository(t)
	worktree := filepath.Join(manager.Root, "candidate-link")
	ctx := context.Background()
	_, err := manager.Toolchain.CreateWorktree(ctx, manager.Repository, worktree, "HEAD")
	require.NoError(t, err)
	defer func() { _, _ = manager.Toolchain.RemoveWorktree(ctx, manager.Repository, worktree) }()

	require.NoError(t, rejectProtectedChanges(ctx, manager.Toolchain, worktree))
	require.NoError(t, os.Remove(filepath.Join(worktree, "internal", "tool", "tool.go")))
	require.NoError(t, rejectProtectedChanges(ctx, manager.Toolchain, worktree), "deleting ordinary source is allowed")

	require.NoError(t, os.Symlink("go.mod", filepath.Join(worktree, "mod.go")))
	err = rejectProtectedChanges(ctx, manager.Toolchain, worktree)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"mod.go" as a symbolic link`)
}

// A directory symlink the target already tracks turns a harmless name into a
// protected one: fixtures/golden.txt is testdata/golden.txt. No header names
// testdata, strict git apply refuses to write beyond the link, and BSD patch,
// the fallback on macOS, follows it and rewrites the golden file. Only the
// post-apply check sees that. GNU patch refuses to follow the link instead, in
// which case the apply error ends the candidate; either way nothing reaches a
// build and the worktree is gone.
func TestPrepareRejectsAProtectedFileReachedThroughATrackedSymlink(t *testing.T) {
	manager, repo := preparedRepository(t)
	require.NoError(t, os.Symlink("testdata", filepath.Join(repo, "fixtures")))
	for _, args := range [][]string{{"add", "fixtures"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "link"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	patch := "--- a/fixtures/golden.txt\n+++ b/fixtures/golden.txt\n@@ -1 +1 @@\n-hello\n+hi\n"
	_, err := ValidateUnifiedDiff(patch, Policy{})
	require.NoError(t, err, "no header names a protected path, so validation cannot be what stops this")

	_, err = manager.Prepare(context.Background(), "HEAD", writePatch(t, patch), "", Policy{})
	require.Error(t, err)
	if strings.Contains(err.Error(), "applied patch changed a protected path") {
		require.Contains(t, err.Error(), `test file "testdata/golden.txt" is off-limits`)
	} else {
		require.Contains(t, err.Error(), "beyond a symbolic link", "only a patch tool that refused the link may end the candidate some other way")
	}
	entries, readErr := os.ReadDir(manager.Root)
	require.NoError(t, readErr)
	require.Empty(t, entries, "a rejected candidate's worktree is removed")
}
