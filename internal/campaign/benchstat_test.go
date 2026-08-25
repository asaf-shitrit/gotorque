package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/toolchain"
)

// cannedBenchstatExecutor answers every invocation with fixed output so the
// comparison path can be exercised without the benchstat binary installed.
type cannedBenchstatExecutor struct {
	stdout string
	calls  int
}

func (f *cannedBenchstatExecutor) Run(_ context.Context, in toolchain.Invocation) (toolchain.Result, error) {
	f.calls++
	return toolchain.Result{Stdout: []byte(f.stdout), ExitCode: 0}, nil
}

const modernBenchstatOutput = `goos: darwin
goarch: arm64
pkg: example.com/target
cpu: Apple M2
        │ base.txt    │           cand.txt           │
        │   sec/op    │   sec/op     vs base         │
Work-8    1.500µ ± 2%   1.200µ ± 3%  -20.00% (p=0.001 n=7)
Geomean   1.500µ        1.200µ       -20.00%`

const legacyBenchstatOutput = `benchmark              old ns/op     new ns/op     delta
BenchmarkWork-8        1500          1200          -20.00%
BenchmarkGeom-8        1500          1230          -18.00%`

func TestParseBenchstatOutputModern(t *testing.T) {
	summary, ok := parseBenchstatOutput(modernBenchstatOutput)
	if !ok {
		t.Fatal("modern output should parse")
	}
	if !summary.HasPValue {
		t.Fatal("expected p-value extraction")
	}
	if summary.PValue != 0.001 {
		t.Fatalf("p-value = %v, want 0.001", summary.PValue)
	}
	if summary.InformativeOnly {
		t.Fatal("p-value present must not be informative-only")
	}
	if !summary.supported() {
		t.Fatal("p=0.001 < alpha should be supported")
	}
	if summary.DeltaPercent != -20.00 {
		t.Fatalf("delta = %v, want -20", summary.DeltaPercent)
	}
}

func TestParseBenchstatOutputLegacy(t *testing.T) {
	summary, ok := parseBenchstatOutput(legacyBenchstatOutput)
	if !ok {
		t.Fatal("legacy delta output should parse")
	}
	if summary.HasPValue || summary.supported() {
		t.Fatal("legacy format has no p-value and must never grant support")
	}
	if !summary.InformativeOnly {
		t.Fatal("legacy output should be informative-only")
	}
	if summary.DeltaPercent != -18.00 {
		t.Fatalf("delta = %v, want -18 (last delta line wins)", summary.DeltaPercent)
	}
}

func TestParseBenchstatOutputUnparseable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"empty", ""},
		{"usage error", "usage: benchstat [-options] old.txt new.txt\n"},
		{"headers only", "goos: darwin\ngoarch: arm64\npkg: example.com/x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseBenchstatOutput(tc.output); ok {
				t.Fatalf("output %q should not parse", tc.output)
			}
		})
	}
}

func TestParseBenchstatOutputConservativePValue(t *testing.T) {
	output := "A-8  1µ ± 1%  0.5µ ± 1%  -50.00% (p=0.001 n=7)\n" +
		"B-8  2µ ± 1%  2.1µ ± 1%  +5.00% (p=0.400 n=7)"
	summary, ok := parseBenchstatOutput(output)
	if !ok {
		t.Fatal("multi-row output should parse")
	}
	if summary.PValue != 0.400 {
		t.Fatalf("p-value = %v, want most conservative 0.400", summary.PValue)
	}
	if summary.supported() {
		t.Fatal("any row above alpha must withhold support")
	}
}

func TestParseBenchstatIgnoresStderrNoisePercent(t *testing.T) {
	// Only ± sample-noise percentages present: no delta, no p-value -> unparseable.
	if _, ok := parseBenchstatOutput("Work-8  1.5µ ± 2%  1.4µ ± 3%"); ok {
		t.Fatal("± noise alone must not count as a delta")
	}
}

func wallRuns(vals ...float64) []domain.RunResult {
	runs := make([]domain.RunResult, 0, len(vals))
	for _, v := range vals {
		runs = append(runs, domain.RunResult{Metrics: []domain.Metric{{Name: "wall_time_ns", Value: v}}})
	}
	return runs
}

func benchstatEnabledEngine(t *testing.T, exec toolchain.Executor, benchstatPath string) *Engine {
	t.Helper()
	dir := t.TempDir()
	e := &Engine{dir: dir, toolchain: toolchain.New(toolchain.Options{Executor: exec, BenchstatPath: benchstatPath})}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return e
}

// installFakeBenchstat creates an executable placeholder file so LookPath
// succeeds while the injected executor supplies the actual output.
func installFakeBenchstat(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "benchstat")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompareWallTimeMetricWithBenchstat(t *testing.T) {
	fake := &cannedBenchstatExecutor{stdout: modernBenchstatOutput}
	e := benchstatEnabledEngine(t, fake, installFakeBenchstat(t))

	base := wallRuns(1500, 1520, 1480, 1510, 1495, 1505, 1500)
	cand := wallRuns(1200, 1210, 1195, 1205, 1200, 1198, 1207)
	comparisons, output := e.compareWallTimeMetric(context.Background(), "wid", base, cand)

	if len(comparisons) != 1 {
		t.Fatalf("comparisons = %d, want 1", len(comparisons))
	}
	c := comparisons[0]
	if c.Name != "wid/wall_time_ns" || !c.StatisticallyFit {
		t.Fatalf("expected supported comparison, got %+v", c)
	}
	if c.Confidence <= 0 || c.Confidence >= 1 {
		t.Fatalf("confidence = %v, want 1-p", c.Confidence)
	}
	if fake.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", fake.calls)
	}
	if !strings.Contains(output, "p=0.001") {
		t.Fatalf("raw output missing p-value: %q", output)
	}
	// Sample files land under <campaign>/benchstat/.
	for _, name := range []string{"wid.base.txt", "wid.cand.txt"} {
		data, err := os.ReadFile(filepath.Join(e.dir, "benchstat", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(data), "ns\n") {
			t.Fatalf("%s content unexpected: %q", name, string(data))
		}
	}
}

func TestCompareWallTimeMetricInsignificantPWithdrawsSupport(t *testing.T) {
	fake := &cannedBenchstatExecutor{stdout: "Work-8  100ns ± 40%  101ns ± 41%  +1.00% (p=0.912 n=7)"}
	e := benchstatEnabledEngine(t, fake, installFakeBenchstat(t))

	base := wallRuns(100, 140, 90, 110, 95, 105, 100)
	cand := wallRuns(101, 141, 91, 111, 96, 106, 101)
	comparisons, _ := e.compareWallTimeMetric(context.Background(), "wid", base, cand)
	if comparisons[0].StatisticallyFit {
		t.Fatalf("insignificant p-value must withdraw support: %+v", comparisons[0])
	}
}

func TestCompareWallTimeMetricFallsBackWithoutBinary(t *testing.T) {
	fake := &cannedBenchstatExecutor{stdout: modernBenchstatOutput}
	e := benchstatEnabledEngine(t, fake, "gotorque-no-such-benchstat-binary")

	base := wallRuns(100, 102, 99, 101, 100, 98, 101)
	cand := wallRuns(50, 51, 49, 50, 52, 48, 51)
	comparisons, output := e.compareWallTimeMetric(context.Background(), "wid", base, cand)

	if fake.calls != 0 {
		t.Fatalf("missing binary must skip invocation, calls = %d", fake.calls)
	}
	if output != "" {
		t.Fatalf("no benchstat output expected, got %q", output)
	}
	if !comparisons[0].StatisticallyFit {
		t.Fatalf("t-test path unchanged; expected support: %+v", comparisons[0])
	}
}

func TestCompareWallTimeMetricFallsBackOnUnparseableOutput(t *testing.T) {
	fake := &cannedBenchstatExecutor{stdout: "benchstat: no common benchmarks"}
	e := benchstatEnabledEngine(t, fake, installFakeBenchstat(t))

	base := wallRuns(100, 102, 99, 101, 100, 98, 101)
	cand := wallRuns(50, 51, 49, 50, 52, 48, 51)
	comparisons, output := e.compareWallTimeMetric(context.Background(), "wid", base, cand)

	if output != "" || comparisons[0].Confidence != 0 {
		t.Fatalf("unparseable output must leave comparison untouched: %+v out=%q", comparisons[0], output)
	}
	if !comparisons[0].StatisticallyFit {
		t.Fatal("t-test fallback should still grant support")
	}
}

func TestRunBenchstatSkipsEmptySamples(t *testing.T) {
	fake := &cannedBenchstatExecutor{stdout: modernBenchstatOutput}
	e := benchstatEnabledEngine(t, fake, installFakeBenchstat(t))
	if _, _, ok := e.runBenchstat(context.Background(), "wid", nil, wallVals(1)); ok || fake.calls != 0 {
		t.Fatal("empty baseline samples must skip benchstat")
	}
}

func wallVals(vals ...float64) []float64 { return vals }

func TestTruncateHeadCapsBenchstatOutput(t *testing.T) {
	long := strings.Repeat("x", benchstatMaxOutputBytes+500)
	got := truncateHead(long, benchstatMaxOutputBytes)
	if len(got) != benchstatMaxOutputBytes {
		t.Fatalf("len = %d, want %d", len(got), benchstatMaxOutputBytes)
	}
}

func TestReportRendersBenchstatBlock(t *testing.T) {
	state := State{
		ID: "c1",
		CandidateRecords: []CandidateRecord{{
			Attempt: 1, CandidateID: "abc", Decision: domain.DecisionAccepted,
			BenchstatOutput: "Work-8  1.5µ  1.2µ  -20.00% (p=0.001 n=7)",
		}},
	}
	rendered := RenderMarkdown(state)
	if !strings.Contains(rendered, "```text\nWork-8  1.5µ  1.2µ  -20.00% (p=0.001 n=7)\n```") {
		t.Fatalf("fenced benchstat block missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "-20.00% (p=0.001 n=7)\n```\n```") {
		t.Fatal("unexpected nested fences")
	}
}

func TestReportOmitsEmptyBenchstatBlock(t *testing.T) {
	state := State{
		CandidateRecords: []CandidateRecord{{Attempt: 1, CandidateID: "abc"}},
	}
	if rendered := RenderMarkdown(state); strings.Contains(rendered, "Benchstat") {
		t.Fatalf("benchstat section rendered without output:\n%s", rendered)
	}
}
