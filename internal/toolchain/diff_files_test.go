package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffFilesRejectsRelativePaths(t *testing.T) {
	chain := New(Options{Executor: &fakeExecutor{}})
	if _, err := chain.DiffFiles(context.Background(), "old.go", "/abs/new.go", "pkg/file.go"); err == nil {
		t.Fatal("expected relative old path rejection")
	}
	if _, err := chain.DiffFiles(context.Background(), "/abs/old.go", "new.go", "pkg/file.go"); err == nil {
		t.Fatal("expected relative new path rejection")
	}
	if _, err := chain.DiffFiles(context.Background(), "/abs/old.go", "/abs/new.go", ""); err == nil {
		t.Fatal("expected empty relPath rejection")
	}
}

// TestDiffFilesProducesRepositoryRelativeHeaders runs a real `git diff
// --no-index` between two temporary files and checks the produced diff names
// its a/ and b/ headers with the repository-relative path, not the temporary
// paths on disk, and applies cleanly with `git apply` against a tree that
// holds the old content at that path.
func TestDiffFilesProducesRepositoryRelativeHeaders(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "before.go")
	newPath := filepath.Join(dir, "after.go")
	oldContent := "package pkg\n\nfunc f() int {\n\treturn 1\n}\n"
	newContent := "package pkg\n\nfunc f() int {\n\treturn 2\n}\n"
	if err := os.WriteFile(oldPath, []byte(oldContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(newContent), 0o600); err != nil {
		t.Fatal(err)
	}

	chain := New(Options{})
	result, err := chain.DiffFiles(context.Background(), oldPath, newPath, "pkg/file.go")
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}
	assertDiffNamesOnlyTheRelativePath(t, result.Stdout, dir)
	assertDiffAppliesToExpectedContent(t, chain, result.Stdout, oldContent, newContent)
}

func assertDiffNamesOnlyTheRelativePath(t *testing.T, stdout []byte, dir string) {
	t.Helper()
	diff := string(stdout)
	if strings.Contains(diff, dir) {
		t.Fatalf("diff still names the temporary directory:\n%s", diff)
	}
	for _, want := range []string{"--- a/pkg/file.go", "+++ b/pkg/file.go"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

// assertDiffAppliesToExpectedContent applies stdout to a real repository
// seeded with oldContent at pkg/file.go and checks it lands on newContent.
func assertDiffAppliesToExpectedContent(t *testing.T, chain *Toolchain, stdout []byte, oldContent, newContent string) {
	t.Helper()
	repo := setupGitRepo(t)
	path := filepath.Join(repo, "pkg", "file.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(oldContent), 0o600); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(t.TempDir(), "candidate.diff")
	if err := os.WriteFile(patchPath, stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.ApplyPatch(context.Background(), repo, patchPath); err != nil {
		t.Fatalf("apply generated diff: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newContent {
		t.Fatalf("applied file = %q, want %q", got, newContent)
	}
}

func TestDiffFilesReturnsNoErrorWhenFilesAreIdentical(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "same.go")
	newPath := filepath.Join(dir, "same2.go")
	content := "package pkg\n"
	if err := os.WriteFile(oldPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	chain := New(Options{})
	result, err := chain.DiffFiles(context.Background(), oldPath, newPath, "pkg/file.go")
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}
	if len(result.Stdout) != 0 {
		t.Fatalf("expected empty diff for identical files, got %q", result.Stdout)
	}
}
