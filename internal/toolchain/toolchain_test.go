package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeExecutor struct {
	invocations []Invocation
	result      Result
	err         error
}

func (f *fakeExecutor) Run(_ context.Context, in Invocation) (Result, error) {
	f.invocations = append(f.invocations, in)
	return f.result, f.err
}

func TestBuildUsesAllowlistedGoBuild(t *testing.T) {
	repo := t.TempDir()
	fake := &fakeExecutor{}
	chain := New(Options{Executor: fake, GoPath: "go-test"})
	output := filepath.Join(t.TempDir(), "target")
	if _, err := chain.Build(context.Background(), BuildRequest{Repository: repo, Target: "./cmd/tool", Output: output, Tags: []string{"fast"}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.invocations) != 1 {
		t.Fatalf("invocations = %d", len(fake.invocations))
	}
	got := fake.invocations[0]
	if got.Path != "go-test" || got.Args[0] != "build" || got.Args[len(got.Args)-1] != "./cmd/tool" {
		t.Fatalf("unexpected invocation: %#v", got)
	}
}

// Directory (ADR 0023) points the build's working directory at a nested Go
// module inside the repository, for a target whose CLI keeps its own go.mod
// (alecthomas/chroma's cmd/chroma is one).
func TestBuildRunsFromDirectory(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "chroma"), 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{}
	chain := New(Options{Executor: fake, GoPath: "go-test"})
	output := filepath.Join(t.TempDir(), "chroma")
	if _, err := chain.Build(context.Background(), BuildRequest{Repository: repo, Directory: "cmd/chroma", Target: ".", Output: output}); err != nil {
		t.Fatal(err)
	}
	if len(fake.invocations) != 1 {
		t.Fatalf("invocations = %d", len(fake.invocations))
	}
	want := filepath.Join(repo, "cmd", "chroma")
	if fake.invocations[0].Dir != want {
		t.Fatalf("build dir = %q, want %q", fake.invocations[0].Dir, want)
	}
}

func TestBuildEmptyDirectoryRunsFromRepository(t *testing.T) {
	repo := t.TempDir()
	fake := &fakeExecutor{}
	chain := New(Options{Executor: fake, GoPath: "go-test"})
	output := filepath.Join(t.TempDir(), "tool")
	if _, err := chain.Build(context.Background(), BuildRequest{Repository: repo, Target: "./cmd/tool", Output: output}); err != nil {
		t.Fatal(err)
	}
	if fake.invocations[0].Dir != repo {
		t.Fatalf("build dir = %q, want %q", fake.invocations[0].Dir, repo)
	}
}

func TestBuildRejectsUnsafeDirectory(t *testing.T) {
	repo := t.TempDir()
	chain := New(Options{Executor: &fakeExecutor{}})
	output := filepath.Join(t.TempDir(), "tool")
	if _, err := chain.Build(context.Background(), BuildRequest{Repository: repo, Directory: "/etc", Target: ".", Output: output}); err == nil {
		t.Fatal("expected absolute build directory to be rejected")
	}
	if _, err := chain.Build(context.Background(), BuildRequest{Repository: repo, Directory: "../escape", Target: ".", Output: output}); err == nil {
		t.Fatal("expected repository-escaping build directory to be rejected")
	}
}

func TestTracePprofRejectsUnknownKind(t *testing.T) {
	chain := New(Options{Executor: &fakeExecutor{}})
	if _, err := chain.TracePprof(context.Background(), filepath.Join(t.TempDir(), "trace.out"), "cpu"); err == nil {
		t.Fatal("expected unsupported trace kind error")
	}
}

func TestMergeEnvironmentOverridesOnce(t *testing.T) {
	values := mergeEnvironment(os.Environ(), []string{"GO_AGENT_TEST=value", "GO_AGENT_TEST=final"})
	seen := 0
	for _, value := range values {
		if value == "GO_AGENT_TEST=final" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("GO_AGENT_TEST occurrences = %d", seen)
	}
	_ = os.Getenv("PATH")
}

func TestTestPassesAbsoluteCpuprofile(t *testing.T) {
	repo := t.TempDir()
	fake := &fakeExecutor{}
	chain := New(Options{Executor: fake})
	profile := filepath.Join(t.TempDir(), "cpu.pb.gz")
	if _, err := chain.Test(context.Background(), TestRequest{Repository: repo, Bench: ".", Cpuprofile: profile}); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Test(context.Background(), TestRequest{Repository: repo, Bench: ".", Cpuprofile: "relative/cpu.out"}); err == nil {
		t.Fatal("expected relative cpuprofile path rejection")
	}
	args := fake.invocations[0].Args
	for i, arg := range args {
		if arg == "-cpuprofile" && args[i+1] == profile {
			return
		}
	}
	t.Fatalf("-cpuprofile flag missing: %#v", args)
}

func TestTestPassesAbsoluteMemprofile(t *testing.T) {
	repo := t.TempDir()
	fake := &fakeExecutor{}
	chain := New(Options{Executor: fake})
	profile := filepath.Join(t.TempDir(), "mem.pb.gz")
	if _, err := chain.Test(context.Background(), TestRequest{Repository: repo, Bench: ".", Memprofile: profile}); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Test(context.Background(), TestRequest{Repository: repo, Bench: ".", Memprofile: "relative/mem.out"}); err == nil {
		t.Fatal("expected relative memprofile path rejection")
	}
	args := fake.invocations[0].Args
	for i, arg := range args {
		if arg == "-memprofile" && args[i+1] == profile {
			return
		}
	}
	t.Fatalf("-memprofile flag missing: %#v", args)
}
