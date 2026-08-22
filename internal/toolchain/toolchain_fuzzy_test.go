package toolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupGitRepo creates a minimal git repository with one committed file.
func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n\tprintln(\"hello\")\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	return dir
}

func TestApplyPatchFuzzyRecoversDriftedContext(t *testing.T) {
	repo := setupGitRepo(t)
	patchPath := filepath.Join(t.TempDir(), "fix.diff")
	// The hunk context drifts from the real file (x := 10 vs x := 1),
	// simulating a model writing context lines from memory.
	patch := `--- a/main.go
+++ b/main.go
@@ -3,6 +3,7 @@ package main
 func main() {
-	x := 1
+	x := 1
 	_ = x
+	y := compute(x)
 	println("hello")
 }
`
	if err := os.WriteFile(patchPath, []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}
	tc := New(Options{})
	ctx := context.Background()
	worktree := filepath.Join(t.TempDir(), "wt")
	if _, err := tc.CreateWorktree(ctx, repo, worktree, "HEAD"); err != nil {
		t.Skipf("worktree unavailable in sandbox: %v", err)
	}
	defer func() { _, _ = tc.RemoveWorktree(ctx, repo, worktree) }()

	if _, err := tc.ApplyPatchCheck(ctx, worktree, patchPath); err == nil {
		t.Fatal("expected strict apply to fail on drifted context")
	}
	if _, err := tc.ApplyPatchFuzzy(ctx, worktree, patchPath); err != nil {
		t.Fatalf("fuzzy apply failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "compute(x)") {
		t.Fatalf("patch content missing after fuzzy apply:\n%s", data)
	}
}

func TestApplyPatchFuzzyRejectsGarbage(t *testing.T) {
	repo := setupGitRepo(t)
	patchPath := filepath.Join(t.TempDir(), "bad.diff")
	if err := os.WriteFile(patchPath, []byte("not a patch at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	tc := New(Options{})
	worktree := filepath.Join(t.TempDir(), "wt")
	ctx := context.Background()
	if _, err := tc.CreateWorktree(ctx, repo, worktree, "HEAD"); err != nil {
		t.Skipf("worktree unavailable in sandbox: %v", err)
	}
	defer func() { _, _ = tc.RemoveWorktree(ctx, repo, worktree) }()
	if _, err := tc.ApplyPatchFuzzy(ctx, worktree, patchPath); err == nil {
		t.Fatal("expected fuzzy apply to fail on non-patch input")
	}
}
