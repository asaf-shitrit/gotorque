package toolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestChangedFilesRunsFixedGitStatus(t *testing.T) {
	repo := t.TempDir()
	fake := &fakeExecutor{result: Result{Stdout: []byte(" M main.go\x00?? new.go\x00")}}
	chain := New(Options{Executor: fake, GitPath: "git-test"})
	paths, err := chain.ChangedFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"main.go", "new.go"}) {
		t.Fatalf("paths = %q", paths)
	}
	got := fake.invocations[0]
	want := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored", "--no-renames"}
	if got.Path != "git-test" || got.Dir != repo || !slices.Equal(got.Args, want) {
		t.Fatalf("unexpected invocation: %#v", got)
	}
	if _, err := chain.ChangedFiles(context.Background(), "relative"); err == nil {
		t.Fatal("expected relative repository rejection")
	}
}

func TestParsePorcelainZ(t *testing.T) {
	// Paths are never quoted under -z, so spaces and tabs arrive verbatim, and
	// a rename carries its source as the following field.
	paths, err := parsePorcelainZ([]byte("R  moved.go\x00vendor/x/orig.go\x00 D go.sum\x00?? dir/a b.go\x00!! out.log\x00C  copy.go\x00src.go\x00"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"moved.go", "vendor/x/orig.go", "go.sum", "dir/a b.go", "out.log", "copy.go", "src.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
	if paths, err := parsePorcelainZ(nil); err != nil || len(paths) != 0 {
		t.Fatalf("empty status = %q, %v", paths, err)
	}
	for _, bad := range []string{"M\x00", "MM_main.go\x00", "R  moved.go\x00", "R  moved.go\x00\x00"} {
		if _, err := parsePorcelainZ([]byte(bad)); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

// The authoritative view has to see what a real checkout reports, including
// the files a .gitignore would otherwise hide.
func TestChangedFilesReadsARealWorktree(t *testing.T) {
	repo := setupGitRepo(t)
	gitRun := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, content string) {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "ignored/\n")
	write("go.mod", "module x\n")
	gitRun("add", "-A")
	gitRun("commit", "-qm", "more")

	chain := New(Options{})
	paths, err := chain.ChangedFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("clean checkout reported %q", paths)
	}

	if err := os.Remove(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	write("main.go", "package main\n")
	write("ignored/testdata/golden.txt", "rewritten\n")
	write("pkg/new_test.go", "package pkg\n")
	paths, err = chain.ChangedFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	want := []string{"go.mod", "ignored/testdata/golden.txt", "main.go", "pkg/new_test.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}
