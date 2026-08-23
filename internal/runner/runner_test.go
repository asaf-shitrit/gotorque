package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/toolchain"
)

type fakeExecutor struct{ calls []toolchain.Invocation }

func (f *fakeExecutor) Run(_ context.Context, in toolchain.Invocation) (toolchain.Result, error) {
	f.calls = append(f.calls, in)
	return toolchain.Result{Stdout: []byte("stable output"), Started: time.Unix(0, 0), Duration: 10 * time.Millisecond}, nil
}

func TestRunInterleavesBaselineAndCandidate(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{}
	r, err := New(Options{Executor: fake, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes")})
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Unix(1, 0).UTC() }
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(binary, []byte("placeholder"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload := domain.Workload{ID: "w", Command: domain.Command{Args: []string{"--input", "sample"}}}
	baseline := RunRequest{Build: Build{ID: "base", BinaryPath: binary}, Workload: workload, Mode: domain.RunModeMeasurement, NetworkAllowed: true, FilesystemAllowed: true}
	candidate := RunRequest{Build: Build{ID: "candidate", BinaryPath: binary}, Workload: workload, Mode: domain.RunModeMeasurement, NetworkAllowed: true, FilesystemAllowed: true}
	result, err := r.RunInterleaved(context.Background(), ABRequest{Baseline: baseline, Candidate: candidate, Repetitions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Baseline) != 2 || len(result.Candidate) != 2 {
		t.Fatalf("unexpected result sizes: %#v", result)
	}
	if len(fake.calls) != 4 {
		t.Fatalf("calls = %d", len(fake.calls))
	}
	if result.Baseline[0].StdoutDigest != Digest([]byte("stable output")) {
		t.Fatal("stdout digest was not captured")
	}
}

func TestRunUsesPolicyMetricNames(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{Executor: &fakeExecutor{}, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes")})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(binary, []byte("placeholder"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), RunRequest{
		Build: Build{ID: "base", BinaryPath: binary}, Workload: domain.Workload{ID: "w"},
		Mode: domain.RunModeMeasurement, NetworkAllowed: true, FilesystemAllowed: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(result.Metrics))
	for _, metric := range result.Metrics {
		names = append(names, metric.Name)
	}
	for _, want := range []string{"wall_time_ns", "cpu_time_ns", "peak_memory_bytes"} {
		if !contains(names, want) {
			t.Fatalf("metrics %v do not contain %q", names, want)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSandboxFailsClosedWithoutNetworkGuard(t *testing.T) {
	_, err := NewSandbox(SandboxOptions{Root: t.TempDir(), NetworkDisabled: true})
	if err == nil {
		t.Fatal("expected network guard error")
	}
}
