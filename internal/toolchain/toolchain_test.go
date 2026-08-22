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

func TestTracePprofRejectsUnknownKind(t *testing.T) {
	chain := New(Options{Executor: &fakeExecutor{}})
	if _, err := chain.TracePprof(context.Background(), filepath.Join(t.TempDir(), "trace.out"), "cpu"); err == nil {
		t.Fatal("expected unsupported trace kind error")
	}
}

func TestMergeEnvironmentOverridesOnce(t *testing.T) {
	values := mergeEnvironment([]string{"GO_AGENT_TEST=value", "GO_AGENT_TEST=final"})
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
