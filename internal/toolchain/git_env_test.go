package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Git must resolve the repository it was pointed at with Dir, never one the
// caller's environment names. Inside a git hook git exports GIT_INDEX_FILE and
// GIT_PREFIX, and a caller working in another checkout may export GIT_DIR or
// GIT_WORK_TREE. Before those were stripped, a command aimed at a clean target
// reported the caller's unrelated repository instead, so candidate worktree
// preparation failed and tests skipped over it.
func TestGitCommandsIgnoreInheritedRepoScoping(t *testing.T) {
	// The unrelated repository is deliberately dirty, so following its index
	// or worktree instead of the target is observable in the output.
	elsewhere := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(elsewhere, "untracked.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := setupGitRepo(t)

	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(elsewhere, ".git", "index"))
	t.Setenv("GIT_PREFIX", "")

	res, err := New(Options{}).GitStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("GitStatus: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "" {
		t.Fatalf("GitStatus against a clean target reported %q: git followed the caller's GIT_* instead of Dir", got)
	}
}
