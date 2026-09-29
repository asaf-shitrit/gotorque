package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/toolchain"
	"github.com/stretchr/testify/require"
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
	if err := os.WriteFile(binary, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload := domain.Workload{ID: "w", Command: domain.Command{Args: []string{"--input", "sample"}}}
	baseline := RunRequest{Build: Build{ID: "base", BinaryPath: binary}, Workload: workload, Mode: domain.RunModeMeasurement}
	candidate := RunRequest{Build: Build{ID: "candidate", BinaryPath: binary}, Workload: workload, Mode: domain.RunModeMeasurement}
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
	if err := os.WriteFile(binary, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), RunRequest{
		Build: Build{ID: "base", BinaryPath: binary}, Workload: domain.Workload{ID: "w"},
		Mode: domain.RunModeMeasurement,
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

// TestRunInterleavedAlternatesWhichSideRunsFirst pins the ABBA order: pairs
// alternate baseline-first and candidate-first, and each result still lands
// on its own side.
func TestRunInterleavedAlternatesWhichSideRunsFirst(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	require.NoError(t, err)
	fake := &fakeExecutor{}
	r, err := New(Options{Executor: fake, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes")})
	require.NoError(t, err)
	baseBinary, candBinary := filepath.Join(root, "base-bin"), filepath.Join(root, "cand-bin")
	require.NoError(t, os.WriteFile(baseBinary, []byte("placeholder"), 0o600))
	require.NoError(t, os.WriteFile(candBinary, []byte("placeholder"), 0o600))
	workload := domain.Workload{ID: "w"}
	baseline := RunRequest{Build: Build{ID: "base", BinaryPath: baseBinary}, Workload: workload, Mode: domain.RunModeMeasurement}
	candidate := RunRequest{Build: Build{ID: "candidate", BinaryPath: candBinary}, Workload: workload, Mode: domain.RunModeMeasurement}
	result, err := r.RunInterleaved(context.Background(), ABRequest{Baseline: baseline, Candidate: candidate, Repetitions: 4})
	require.NoError(t, err)
	require.Equal(t, "BCCBBCCB", runOrder(fake.calls))
	require.Len(t, result.Baseline, 4)
	require.Len(t, result.Candidate, 4)
	for i := range 4 {
		require.Equal(t, "base", result.Baseline[i].BuildID, "pair %d", i)
		require.Equal(t, "candidate", result.Candidate[i].BuildID, "pair %d", i)
	}
}

// runOrder spells the sequence of binaries the executor ran: B for the
// baseline binary, C for the candidate's.
func runOrder(calls []toolchain.Invocation) string {
	var order strings.Builder
	for _, call := range calls {
		line := call.Path + " " + strings.Join(call.Args, " ")
		switch {
		case strings.Contains(line, "base-bin"):
			order.WriteString("B")
		case strings.Contains(line, "cand-bin"):
			order.WriteString("C")
		}
	}
	return order.String()
}

// exitingExecutor exits with a fixed status and, like the real toolchain,
// returns an error for any nonzero one.
type exitingExecutor struct{ code int }

func (e exitingExecutor) Run(_ context.Context, _ toolchain.Invocation) (toolchain.Result, error) {
	result := toolchain.Result{Stdout: []byte("differences"), Started: time.Unix(0, 0), Duration: time.Millisecond, ExitCode: e.code}
	if e.code != 0 {
		return result, errors.New("exit status " + strconv.Itoa(e.code))
	}
	return result, nil
}

// TestRunJudgesExitAgainstTheWorkloadsExpectedStatus: a diff tool exits 1
// when its inputs differ, and that run worked. Every nonzero exit used to
// fail the run, so such a target could not be measured.
func TestRunJudgesExitAgainstTheWorkloadsExpectedStatus(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	require.NoError(t, err)
	binary := filepath.Join(root, "binary")
	require.NoError(t, os.WriteFile(binary, []byte("placeholder"), 0o600))
	run := func(exit, expected int) (domain.RunResult, error) {
		r, err := New(Options{Executor: exitingExecutor{code: exit}, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes")})
		require.NoError(t, err)
		return r.Run(context.Background(), RunRequest{Build: Build{ID: "b", BinaryPath: binary}, Workload: domain.Workload{ID: "w", ExpectedExitCode: expected}, Mode: domain.RunModeMeasurement})
	}

	result, err := run(1, 1)
	require.NoError(t, err, "exit 1 is what this workload does")
	require.Equal(t, 1, result.ExitCode)
	require.Empty(t, result.Error)

	_, err = run(1, 0)
	require.ErrorContains(t, err, "exit status 1", "an undeclared nonzero exit still fails")

	_, err = run(0, 1)
	require.ErrorContains(t, err, "exited with status 0, but the workload expects 1")

	_, err = run(2, 1)
	require.ErrorContains(t, err, "exit status 2", "a different nonzero status is a failure")
}
