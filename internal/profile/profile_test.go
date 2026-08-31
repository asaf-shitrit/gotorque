package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"example.com/gotorque/internal/runner"
	"example.com/gotorque/internal/toolchain"
)

// sequenceExecutor returns one queued result per call, in order, so tests can
// distinguish the pprof-top call from the trace-conversion call.
type sequenceExecutor struct {
	results []toolchain.Result
	errs    []error
	calls   []toolchain.Invocation
}

func (s *sequenceExecutor) Run(_ context.Context, in toolchain.Invocation) (toolchain.Result, error) {
	s.calls = append(s.calls, in)
	i := len(s.calls) - 1
	var result toolchain.Result
	var err error
	if i < len(s.results) {
		result = s.results[i]
	}
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return result, err
}

const validTopReport = `Showing nodes accounting for 90ms, 90% of 100ms total
      flat  flat%   sum%        cum   cum%
     50ms 50.00% 50.00%      70ms 70.00%  example.com/parser.Decode
`

func newTestCollector(t *testing.T, exec toolchain.Executor) Collector {
	t.Helper()
	store, err := runner.NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Collector{
		Toolchain: toolchain.New(toolchain.Options{Executor: exec}),
		Artifacts: store,
		TempRoot:  t.TempDir(),
	}
}

func TestSummarizePprofRequiresToolchainAndArtifacts(t *testing.T) {
	if _, err := (Collector{}).SummarizePprof(context.Background(), "/abs/profile.pb.gz", 10); err == nil {
		t.Fatal("expected error when toolchain and artifacts are missing")
	}
}

func TestSummarizePprofRequiresAbsolutePath(t *testing.T) {
	c := newTestCollector(t, &sequenceExecutor{})
	if _, err := c.SummarizePprof(context.Background(), "relative/profile.pb.gz", 10); err == nil {
		t.Fatal("expected error for non-absolute profile path")
	}
}

func TestSummarizePprofPropagatesToolchainError(t *testing.T) {
	c := newTestCollector(t, &sequenceExecutor{errs: []error{errors.New("pprof failed")}})
	if _, err := c.SummarizePprof(context.Background(), filepath.Join(t.TempDir(), "p.pb.gz"), 10); err == nil {
		t.Fatal("expected error propagated from PprofTop")
	}
}

func TestSummarizePprofHappyPath(t *testing.T) {
	exec := &sequenceExecutor{results: []toolchain.Result{{Stdout: []byte(validTopReport)}}}
	c := newTestCollector(t, exec)
	profilePath := filepath.Join(t.TempDir(), "p.pb.gz")
	summary, err := c.SummarizePprof(context.Background(), profilePath, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.SourcePath != profilePath {
		t.Fatalf("SourcePath = %q, want %q", summary.SourcePath, profilePath)
	}
	if summary.RawReport == "" {
		t.Fatal("expected raw report artifact path")
	}
	if len(summary.Functions) != 1 || summary.Functions[0].Name != "example.com/parser.Decode" {
		t.Fatalf("unexpected functions: %#v", summary.Functions)
	}
}

func TestSummarizeTraceRequiresToolchainAndArtifacts(t *testing.T) {
	if _, err := (Collector{}).SummarizeTrace(context.Background(), "/abs/trace.out", "net", 10); err == nil {
		t.Fatal("expected error when toolchain and artifacts are missing")
	}
}

func TestSummarizeTraceRequiresAbsolutePath(t *testing.T) {
	c := newTestCollector(t, &sequenceExecutor{})
	if _, err := c.SummarizeTrace(context.Background(), "relative/trace.out", "net", 10); err == nil {
		t.Fatal("expected error for non-absolute trace path")
	}
}

func TestSummarizeTracePropagatesConversionError(t *testing.T) {
	c := newTestCollector(t, &sequenceExecutor{errs: []error{errors.New("trace conversion failed")}})
	if _, err := c.SummarizeTrace(context.Background(), filepath.Join(t.TempDir(), "trace.out"), "net", 10); err == nil {
		t.Fatal("expected error propagated from TracePprof")
	}
}

func TestSummarizeTracePropagatesSummarizeError(t *testing.T) {
	exec := &sequenceExecutor{
		results: []toolchain.Result{{Stdout: []byte("converted-pprof-bytes")}},
		errs:    []error{nil, errors.New("pprof top failed")},
	}
	c := newTestCollector(t, exec)
	if _, err := c.SummarizeTrace(context.Background(), filepath.Join(t.TempDir(), "trace.out"), "net", 10); err == nil {
		t.Fatal("expected wrapped error from SummarizePprof")
	}
}

func TestSummarizeTraceHappyPath(t *testing.T) {
	exec := &sequenceExecutor{results: []toolchain.Result{
		{Stdout: []byte("converted-pprof-bytes")},
		{Stdout: []byte(validTopReport)},
	}}
	c := newTestCollector(t, exec)
	tracePath := filepath.Join(t.TempDir(), "trace.out")
	summary, err := c.SummarizeTrace(context.Background(), tracePath, "sync", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Kind != "sync" {
		t.Fatalf("Kind = %q, want sync", summary.Kind)
	}
	if summary.RawProfile == "" {
		t.Fatal("expected raw profile artifact path")
	}
	if len(summary.Profile.Functions) != 1 {
		t.Fatalf("unexpected profile functions: %#v", summary.Profile.Functions)
	}
	// The temp profile written for the intermediate pprof read must not
	// survive the call.
	entries, err := os.ReadDir(c.TempRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".gz" {
			t.Fatalf("temp trace profile %q was not cleaned up", e.Name())
		}
	}
}

func TestTraceTempRootDefaultsToOSTempDir(t *testing.T) {
	c := Collector{}
	root, err := c.traceTempRoot()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != os.TempDir() {
		t.Fatalf("root = %q, want %q", root, os.TempDir())
	}
}

func TestTraceTempRootRejectsRelativePath(t *testing.T) {
	c := Collector{TempRoot: "relative/dir"}
	if _, err := c.traceTempRoot(); err == nil {
		t.Fatal("expected error for relative temp root")
	}
}

func TestTraceTempRootCreatesDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "temp")
	c := Collector{TempRoot: root}
	got, err := c.traceTempRoot()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != root {
		t.Fatalf("root = %q, want %q", got, root)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("expected directory to be created: %v", err)
	}
}

func TestWriteTempProfileWritesContent(t *testing.T) {
	root := t.TempDir()
	path, err := writeTempProfile(root, []byte("payload"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("content = %q, want payload", data)
	}
}

func TestWriteTempProfileRejectsMissingRoot(t *testing.T) {
	if _, err := writeTempProfile(filepath.Join(t.TempDir(), "does-not-exist"), []byte("x")); err == nil {
		t.Fatal("expected error for missing root directory")
	}
}

func TestRequireReady(t *testing.T) {
	if err := (Collector{}).requireReady(); err == nil {
		t.Fatal("expected error when toolchain and artifacts are missing")
	}
	store, err := runner.NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := Collector{Toolchain: toolchain.New(toolchain.Options{Executor: &sequenceExecutor{}}), Artifacts: store}
	if err := c.requireReady(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseTop(t *testing.T) {
	report := `Showing nodes accounting for 90ms, 90% of 100ms total
      flat  flat%   sum%        cum   cum%
     50ms 50.00% 50.00%      70ms 70.00%  example.com/parser.Decode
     40ms 40.00% 90.00%      40ms 40.00%  runtime.mallocgc
`
	summary := parseTop(report)
	if summary.Total != "100ms total" {
		t.Fatalf("total = %q", summary.Total)
	}
	if len(summary.Functions) != 2 {
		t.Fatalf("function count = %d", len(summary.Functions))
	}
	if summary.Functions[0].Name != "example.com/parser.Decode" {
		t.Fatalf("name = %q", summary.Functions[0].Name)
	}
	if summary.Functions[0].CumulativePercent != 70 {
		t.Fatalf("cumulative percent = %v", summary.Functions[0].CumulativePercent)
	}
}
