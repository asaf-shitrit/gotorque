package campaign

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/runner"
)

func TestMean(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
		wantOK bool
	}{
		{"empty", nil, 0, false},
		{"empty slice", []float64{}, 0, false},
		{"single", []float64{5}, 5, true},
		{"multiple", []float64{1, 2, 3, 4}, 2.5, true},
		{"negatives", []float64{-2, 2}, 0, true},
		{"fractional", []float64{0.1, 0.2}, 0.15000000000000002, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mean(tc.values)
			if ok != tc.wantOK {
				t.Fatalf("mean(%v) ok = %v, want %v", tc.values, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("mean(%v) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestVariance(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{42}, 0},
		{"constant two", []float64{3, 3}, 0},
		{"two values", []float64{1, 3}, 2},
		{"sample variance", []float64{2, 4, 4, 4, 5, 5, 7, 9}, 32.0 / 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := variance(tc.values)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("variance(%v) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestStatisticallySupported(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want bool
	}{
		{
			name: "too few samples baseline",
			a:    []float64{1, 2, 3},
			b:    []float64{10, 20, 30, 40},
			want: false,
		},
		{
			name: "too few samples candidate",
			a:    []float64{1, 2, 3, 4},
			b:    []float64{10, 20, 30},
			want: false,
		},
		{
			name: "both empty",
			a:    nil,
			b:    nil,
			want: false,
		},
		{
			name: "identical constant samples equal means",
			a:    []float64{100, 100, 100, 100},
			b:    []float64{100, 100, 100, 100},
			want: false,
		},
		{
			name: "identical constant samples different means (degenerate zero variance)",
			a:    []float64{1, 1, 1, 1},
			b:    []float64{2, 2, 2, 2},
			want: true, // se2 == 0 and means differ; helper reports support
		},
		{
			name: "large delta clearly supported",
			a:    []float64{100, 102, 99, 101, 100, 98, 101},
			b:    []float64{200, 198, 202, 199, 201, 197, 203},
			want: true,
		},
		{
			name: "noise not supported",
			a:    []float64{100, 105, 95, 103, 97, 102, 98},
			b:    []float64{101, 96, 104, 99, 102, 97, 100},
			want: false,
		},
		{
			name: "moderate shift with low variance supported",
			a:    []float64{10, 10.1, 9.9, 10, 10.05, 9.95, 10},
			b:    []float64{11, 11.1, 10.9, 11, 11.05, 10.95, 11},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statisticallySupported(tc.a, tc.b); got != tc.want {
				t.Fatalf("statisticallySupported(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestPercentDelta(t *testing.T) {
	tests := []struct {
		name                string
		baseline, candidate float64
		want                float64
	}{
		{"zero baseline", 0, 100, 0},
		{"zero baseline zero candidate", 0, 0, 0},
		{"no change", 50, 50, 0},
		{"doubling", 50, 100, 100},
		{"halving", 100, 50, -50},
		{"increase", 200, 250, 25},
		{"negative result", 100, 0, -100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := percentDelta(tc.baseline, tc.candidate)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("percentDelta(%v, %v) = %v, want %v", tc.baseline, tc.candidate, got, tc.want)
			}
		})
	}
}

func runWithMetric(name string, value float64) domain.RunResult {
	return domain.RunResult{Metrics: []domain.Metric{{Name: name, Value: value}}}
}

func TestCollectMetric(t *testing.T) {
	wallRuns := func(vals ...float64) []domain.RunResult {
		runs := make([]domain.RunResult, 0, len(vals))
		for _, v := range vals {
			runs = append(runs, runWithMetric("wall_time_ns", v))
		}
		return runs
	}

	t.Run("empty runs", func(t *testing.T) {
		got := collectMetric(nil, wallTime)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("selects matching metric only", func(t *testing.T) {
		runs := []domain.RunResult{
			{Metrics: []domain.Metric{
				{Name: "cpu_time_ns", Value: 999},
				{Name: "wall_time_ns", Value: 10},
				{Name: "peak_memory_bytes", Value: 555},
				{Name: "wall_time_ns", Value: 20},
			}},
		}
		// Selector returns only the FIRST matching metric per run.
		got := collectMetric(runs, wallTime)
		if len(got) != 1 || got[0] != 10 {
			t.Fatalf("got %v, want [10]", got)
		}
	})

	t.Run("missing metric skipped", func(t *testing.T) {
		got := collectMetric([]domain.RunResult{{}}, wallTime)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("NaN and Inf filtered", func(t *testing.T) {
		got := collectMetric(wallRuns(math.NaN(), 5, math.Inf(1), 7, math.Inf(-1)), wallTime)
		if len(got) != 2 || got[0] != 5 || got[1] != 7 {
			t.Fatalf("got %v, want [5 7]", got)
		}
	})
}

func TestMetricSelectors(t *testing.T) {
	r := domain.RunResult{Metrics: []domain.Metric{
		{Name: "wall_time_ns", Value: 1},
		{Name: "cpu_time_ns", Value: 2},
		{Name: "peak_memory_bytes", Value: 3},
	}}
	if v, ok := wallTime(r); !ok || v != 1 {
		t.Fatalf("wallTime = %v %v", v, ok)
	}
	if v, ok := cpuTime(r); !ok || v != 2 {
		t.Fatalf("cpuTime = %v %v", v, ok)
	}
	if v, ok := peakMemory(r); !ok || v != 3 {
		t.Fatalf("peakMemory = %v %v", v, ok)
	}
	if _, ok := wallTime(domain.RunResult{}); ok {
		t.Fatal("wallTime should report missing metric")
	}
}

func metricRuns(name string, vals ...float64) []domain.RunResult {
	runs := make([]domain.RunResult, 0, len(vals))
	for _, v := range vals {
		runs = append(runs, runWithMetric(name, v))
	}
	return runs
}

func TestCompareMetric(t *testing.T) {
	t.Run("missing baseline metric yields bare comparison", testCompareMetricMissingBaseline)
	t.Run("zero-mean baseline has no percent delta and is confidently unchanged", testCompareMetricZeroMean)
	t.Run("clear improvement is statistically fit with correct delta", testCompareMetricImprovement)
	t.Run("noise not marked fit", testCompareMetricNoise)
}

func testCompareMetricMissingBaseline(t *testing.T) {
	got := compareMetric("wl", "wall_time_ns", "ns", []domain.RunResult{{}}, metricRuns("wall_time_ns", 1, 2), wallTime)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	c := got[0]
	if (c.Metric != "wall_time_ns" || c.Workload != "wl") || c.Unit != "ns" || c.Baseline != 0 || c.Candidate != 0 || c.DeltaPercent != 0 || c.StatisticallyFit {
		t.Fatalf("unexpected comparison %+v", c)
	}
}

func testCompareMetricZeroMean(t *testing.T) {
	base := metricRuns("peak_memory_bytes", 0, 0, 0, 0)
	// constant zeros: se==0 with equal means -> confidently unchanged,
	// which counts as supported (no regression possible).
	cand := metricRuns("peak_memory_bytes", 0, 0, 0, 0)
	got := compareMetric("w", "peak_memory_bytes", "bytes", base, cand, peakMemory)
	c := got[0]
	if c.Baseline != 0 || c.Candidate != 0 || c.DeltaPercent != 0 || !c.StatisticallyFit {
		t.Fatalf("unexpected comparison %+v", c)
	}
}

func testCompareMetricImprovement(t *testing.T) {
	base := metricRuns("wall_time_ns", 100, 102, 99, 101, 100, 98, 101)
	cand := metricRuns("wall_time_ns", 50, 51, 49, 50, 52, 48, 51)
	got := compareMetric("wid", "wall_time_ns", "ns", base, cand, wallTime)
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	c := got[0]
	if (c.Metric != "wall_time_ns" || c.Workload != "wid") || c.Unit != "ns" {
		t.Fatalf("unexpected name/unit %+v", c)
	}
	if math.Abs(c.Baseline-701.0/7) > 1e-9 || math.Abs(c.Candidate-351.0/7) > 1e-9 {
		t.Fatalf("means wrong: %+v", c)
	}
	wantDelta := percentDelta(701.0/7, 351.0/7)
	if math.Abs(c.DeltaPercent-wantDelta) > 1e-9 {
		t.Fatalf("delta = %v", c.DeltaPercent)
	}
	if !c.StatisticallyFit {
		t.Fatal("expected statistical support")
	}
}

func testCompareMetricNoise(t *testing.T) {
	base := metricRuns("cpu_time_ns", 100, 105, 95, 103, 97, 102, 98)
	cand := metricRuns("cpu_time_ns", 101, 96, 104, 99, 102, 97, 100)
	got := compareMetric("w", "cpu_time_ns", "ns", base, cand, cpuTime)[0]
	if got.StatisticallyFit {
		t.Fatalf("noise should not be statistically fit: %+v", got)
	}
}

func TestBinarySizes(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("ok", func(t *testing.T) {
		a, b, err := binarySizes(write("a", 12), write("b", 34))
		if err != nil || a != 12 || b != 34 {
			t.Fatalf("got (%d, %d, %v)", a, b, err)
		}
	})

	t.Run("missing baseline file", func(t *testing.T) {
		if _, _, err := binarySizes(filepath.Join(dir, "nope"), write("b2", 1)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing candidate file", func(t *testing.T) {
		if _, _, err := binarySizes(write("a3", 1), filepath.Join(dir, "nope")); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("zero-size rejected", func(t *testing.T) {
		if _, _, err := binarySizes(write("empty", 0), write("nonempty", 4)); err == nil {
			t.Fatal("expected error for zero-size binary")
		}
	})
}

func TestTail(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{"empty text", "", 10, ""},
		{"shorter than limit", "abc", 10, "abc"},
		{"exactly limit", "abcdefghij", 10, "abcdefghij"},
		{"longer than limit", "abcdefghijklmnop", 4, "mnop"},
		{"trims whitespace first", "  abc  ", 3, "abc"},
		{"limit larger after trim", " hi ", 100, "hi"},
		{"limit zero on long text", "abcdef", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tail(tc.text, tc.limit); got != tc.want {
				t.Fatalf("tail(%q, %d) = %q, want %q", tc.text, tc.limit, got, tc.want)
			}
		})
	}
}

func TestProhibitedTechniquesFor(t *testing.T) {
	tests := []struct {
		name string
		mode domain.OptimizationPolicy
		want []string
	}{
		{"specialized", domain.PolicySpecialized, []string{"unsafe.", "assembly", "cgo"}},
		{"native", domain.PolicyNative, []string{"cgo"}},
		{"idiomatic", domain.PolicyIdiomatic, nil},
		{"unknown", domain.OptimizationPolicy("bogus"), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := prohibitedTechniquesFor(tc.mode)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Guard against accidental mutation of returned slices by the helpers.
func TestProhibitedTechniquesForReturnsFreshSlice(t *testing.T) {
	a := prohibitedTechniquesFor(domain.PolicySpecialized)
	a[0] = "mutated"
	b := prohibitedTechniquesFor(domain.PolicySpecialized)
	if b[0] == "mutated" {
		t.Fatal("returned slice appears shared across calls")
	}
	if !strings.Contains(strings.Join(b, ","), "unsafe.") {
		t.Fatalf("unexpected content %v", b)
	}
}

func behaviorRun(exitCode int, stdout, sorted string) domain.RunResult {
	return domain.RunResult{ExitCode: exitCode, StdoutDigest: stdout, SortedLinesDigest: sorted}
}

func TestRecordBehaviorMatch(t *testing.T) {
	comparisons := []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "wl", Unit: "ns", Baseline: 1, Candidate: 1}}

	t.Run("no repetitions passes without touching evidence", func(t *testing.T) {
		checkBehaviorNoRepetitions(t, comparisons)
	})

	t.Run("byte-exact equal digests and exits pass", func(t *testing.T) {
		checkBehaviorByteExactEqual(t, comparisons)
	})

	t.Run("byte-exact digest mismatch rejects with repetition and basis", func(t *testing.T) {
		checkBehaviorDigestMismatch(t, comparisons)
	})

	t.Run("byte-exact exit mismatch rejects", func(t *testing.T) {
		checkBehaviorExitMismatch(t, comparisons)
	})

	t.Run("nondeterministic sorted digests agree", func(t *testing.T) {
		checkBehaviorSortedDigestsAgree(t, comparisons)
	})

	t.Run("nondeterministic sorted digest mismatch rejects as order-insensitive", func(t *testing.T) {
		checkBehaviorSortedDigestMismatch(t, comparisons)
	})

	t.Run("nondeterministic empty sorted digest rejects even when bytes match", func(t *testing.T) {
		checkBehaviorEmptySortedDigest(t, comparisons)
	})
}

func checkBehaviorNoRepetitions(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	evidence := orchestrator.CandidateEvidence{}
	if !recordBehaviorMatch(runner.ABResult{}, true, "seed", &evidence, comparisons) {
		t.Fatal("empty A/B result should match")
	}
	if evidence.Summary != "" || evidence.SafetyChecksPassed || evidence.Comparisons != nil {
		t.Fatalf("evidence mutated on success: %+v", evidence)
	}
}

func checkBehaviorByteExactEqual(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "aaa", "a1"), behaviorRun(1, "bbb", "b1")},
		Candidate: []domain.RunResult{behaviorRun(0, "aaa", "a2"), behaviorRun(1, "bbb", "b2")},
	}
	if !recordBehaviorMatch(ab, true, "seed", &orchestrator.CandidateEvidence{}, comparisons) {
		t.Fatal("identical byte-exact runs should match")
	}
}

func checkBehaviorDigestMismatch(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "aaa", "x"), behaviorRun(0, "bbb", "x")},
		Candidate: []domain.RunResult{behaviorRun(0, "aaa", "x"), behaviorRun(0, "ccc", "x")},
	}
	evidence := orchestrator.CandidateEvidence{}
	if recordBehaviorMatch(ab, true, "seed-7", &evidence, comparisons) {
		t.Fatal("digest mismatch should reject")
	}
	if !strings.Contains(evidence.Summary, "byte-exact") || !strings.Contains(evidence.Summary, "seed-7") || !strings.Contains(evidence.Summary, "repetition 2") {
		t.Fatalf("unexpected summary %q", evidence.Summary)
	}
	if !evidence.SafetyChecksPassed {
		t.Fatal("mismatch must flag safety checks passed")
	}
	if len(evidence.Comparisons) != 1 || (evidence.Comparisons[0].Metric != "wall_time_ns" || evidence.Comparisons[0].Workload != "wl") {
		t.Fatalf("comparisons not attached: %+v", evidence.Comparisons)
	}
}

func checkBehaviorExitMismatch(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "same", "s")},
		Candidate: []domain.RunResult{behaviorRun(2, "same", "s")},
	}
	if recordBehaviorMatch(ab, true, "seed", &orchestrator.CandidateEvidence{}, comparisons) {
		t.Fatal("exit mismatch should reject")
	}
}

func checkBehaviorSortedDigestsAgree(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "unsorted-a", "sorted")},
		Candidate: []domain.RunResult{behaviorRun(0, "unsorted-b", "sorted")},
	}
	if !recordBehaviorMatch(ab, false, "seed", &orchestrator.CandidateEvidence{}, comparisons) {
		t.Fatal("equal non-empty sorted digests should match")
	}
}

func checkBehaviorSortedDigestMismatch(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "same-digest", "sorted-a")},
		Candidate: []domain.RunResult{behaviorRun(0, "same-digest", "sorted-b")},
	}
	evidence := orchestrator.CandidateEvidence{}
	if recordBehaviorMatch(ab, false, "seed", &evidence, comparisons) {
		t.Fatal("sorted digest mismatch should reject")
	}
	if !strings.Contains(evidence.Summary, "order-insensitive") {
		t.Fatalf("unexpected summary %q", evidence.Summary)
	}
}

func checkBehaviorEmptySortedDigest(t *testing.T, comparisons []domain.MetricComparison) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "same-digest", "")},
		Candidate: []domain.RunResult{behaviorRun(0, "same-digest", "")},
	}
	if recordBehaviorMatch(ab, false, "seed", &orchestrator.CandidateEvidence{}, comparisons) {
		t.Fatal("empty sorted digest cannot prove order-insensitive equality")
	}
}

func TestPgoBehaviorOK(t *testing.T) {
	t.Run("no repetitions is ok", func(t *testing.T) {
		checkPgoNoRepetitions(t)
	})

	t.Run("matching exit and digest is ok", func(t *testing.T) {
		checkPgoMatchingDigest(t)
	})

	t.Run("exit mismatch reports 1-based repetition", func(t *testing.T) {
		checkPgoExitMismatch(t)
	})

	t.Run("order-insensitive divergence only rejects when sorted digest absent", func(t *testing.T) {
		checkPgoSortedDigestFallback(t)
	})

	t.Run("empty sorted digests on both sides rejects", func(t *testing.T) {
		checkPgoEmptySortedDigests(t)
	})
}

func checkPgoNoRepetitions(t *testing.T) {
	t.Helper()
	ok, rep := pgoBehaviorOK(runner.ABResult{})
	if !ok || rep != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, rep)
	}
}

func checkPgoMatchingDigest(t *testing.T) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "a", ""), behaviorRun(1, "b", "")},
		Candidate: []domain.RunResult{behaviorRun(0, "a", ""), behaviorRun(1, "b", "")},
	}
	if ok, rep := pgoBehaviorOK(ab); !ok || rep != 0 {
		t.Fatalf("got (%v, %d), want (true, 0)", ok, rep)
	}
}

func checkPgoExitMismatch(t *testing.T) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "a", ""), behaviorRun(0, "b", "")},
		Candidate: []domain.RunResult{behaviorRun(0, "a", ""), behaviorRun(3, "b", "")},
	}
	if ok, rep := pgoBehaviorOK(ab); ok || rep != 2 {
		t.Fatalf("got (%v, %d), want (false, 2)", ok, rep)
	}
}

func checkPgoSortedDigestFallback(t *testing.T) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "order-a", "sorted")},
		Candidate: []domain.RunResult{behaviorRun(0, "order-b", "sorted")},
	}
	if ok, _ := pgoBehaviorOK(ab); !ok {
		t.Fatal("stdout digests differ but sorted digests agree; should pass")
	}

	ab.Candidate[0].SortedLinesDigest = ""
	if ok, rep := pgoBehaviorOK(ab); ok || rep != 1 {
		t.Fatalf("got (%v, %d), want (false, 1)", ok, rep)
	}
}

func checkPgoEmptySortedDigests(t *testing.T) {
	t.Helper()
	ab := runner.ABResult{
		Baseline:  []domain.RunResult{behaviorRun(0, "a", "")},
		Candidate: []domain.RunResult{behaviorRun(0, "b", "")},
	}
	if ok, rep := pgoBehaviorOK(ab); ok || rep != 1 {
		t.Fatalf("got (%v, %d), want (false, 1)", ok, rep)
	}
}

// gronAttempt4Ms holds the wall-time samples, in milliseconds, that a campaign
// recorded for a bufio.Writer candidate on gron: a real 20.83% win on the
// workload it affects and a 3.98% regression on a workload roughly half its
// size that has almost nothing to batch. Two representative workloads of
// different scale are precisely the case that broke concatenated pooling.
var (
	gronBigBaseMs   = []float64{22.65, 24.30, 22.80, 23.58, 22.92, 23.50, 25.53}
	gronBigCandMs   = []float64{18.51, 18.48, 19.85, 17.75, 19.21, 18.55, 18.51}
	gronSmallBaseMs = []float64{10.81, 10.26, 10.47, 10.73, 10.66, 10.21, 10.64}
	gronSmallCandMs = []float64{11.25, 10.31, 10.85, 11.38, 10.39, 10.31, 12.23}
)

func TestPooledWorkloadFolding(t *testing.T) {
	t.Run("concatenated samples hide a real improvement", testConcatenatedSamplesHideImprovement)
	t.Run("per-repetition folding keeps the delta and finds support", testFoldingKeepsDeltaAndFindsSupport)
	t.Run("peak memory folds to the high-water mark", testPeakMemoryFoldsToHighWaterMark)
}

func wallRunsMs(series ...[]float64) []domain.RunResult {
	var values []float64
	for _, s := range series {
		for _, v := range s {
			values = append(values, v*1e6)
		}
	}
	return metricRuns("wall_time_ns", values...)
}

func scaledMs(series ...[]float64) [][]float64 {
	scaled := make([][]float64, 0, len(series))
	for _, s := range series {
		row := make([]float64, len(s))
		for i, v := range s {
			row[i] = v * 1e6
		}
		scaled = append(scaled, row)
	}
	return scaled
}

// testConcatenatedSamplesHideImprovement pins the defect the folding fixes:
// concatenating raw samples from workloads with different means inflates the
// pooled spread with between-workload variance (here ~42.6 ms² against ~1 ms²
// of measurement noise), so the test measures the workload mix rather than the
// candidate's effect and a measured 20.83% win reads as no evidence at all.
func testConcatenatedSamplesHideImprovement(t *testing.T) {
	pooled := compareMetric("", "wall_time_ns", "ns",
		wallRunsMs(gronBigBaseMs, gronSmallBaseMs),
		wallRunsMs(gronBigCandMs, gronSmallCandMs), wallTime)[0]
	if pooled.StatisticallyFit {
		t.Fatalf("concatenated pooling claimed support, so this test no longer guards the defect: %+v", pooled)
	}
	affected := compareMetric("big", "wall_time_ns", "ns",
		wallRunsMs(gronBigBaseMs), wallRunsMs(gronBigCandMs), wallTime)[0]
	if !affected.StatisticallyFit {
		t.Fatalf("the affected workload on its own must show support: %+v", affected)
	}
}

func testFoldingKeepsDeltaAndFindsSupport(t *testing.T) {
	folded := compareMetric("", "wall_time_ns", "ns",
		pooledAsRuns(meanPerRepetition(scaledMs(gronBigBaseMs, gronSmallBaseMs)), "wall_time_ns"),
		pooledAsRuns(meanPerRepetition(scaledMs(gronBigCandMs, gronSmallCandMs)), "wall_time_ns"), wallTime)[0]
	concatenated := compareMetric("", "wall_time_ns", "ns",
		wallRunsMs(gronBigBaseMs, gronSmallBaseMs),
		wallRunsMs(gronBigCandMs, gronSmallCandMs), wallTime)[0]

	if !folded.StatisticallyFit {
		t.Fatalf("folded comparison must be supported: %+v", folded)
	}
	if math.Abs(folded.DeltaPercent-concatenated.DeltaPercent) > 0.01 {
		t.Fatalf("folding changed the reported effect: folded %.2f%% vs concatenated %.2f%%", folded.DeltaPercent, concatenated.DeltaPercent)
	}
	if folded.DeltaPercent > -10 {
		t.Fatalf("delta = %.2f%%, want about -13%%", folded.DeltaPercent)
	}
}

func testPeakMemoryFoldsToHighWaterMark(t *testing.T) {
	series := [][]float64{{100, 200, 300}, {150, 120, 90}}
	gotMax := maxPerRepetition(series)
	wantMax := []float64{150, 200, 300}
	gotMean := meanPerRepetition(series)
	wantMean := []float64{125, 160, 195}
	for i := range wantMax {
		if gotMax[i] != wantMax[i] {
			t.Fatalf("maxPerRepetition() = %v, want %v", gotMax, wantMax)
		}
		if gotMean[i] != wantMean[i] {
			t.Fatalf("meanPerRepetition() = %v, want %v", gotMean, wantMean)
		}
	}
	if len(maxPerRepetition(nil)) != 0 || len(meanPerRepetition(nil)) != 0 {
		t.Fatal("folding no workloads must yield no samples")
	}
}

// A patch that touches the gate is refused before any build, through the same
// record every pre-build rejection uses, and its failure detail names the path
// so the optimizer's next proposal can avoid it.
func TestEvaluateCandidateRefusesATestFileEditBeforeBuild(t *testing.T) {
	repo := repositoryWithTestFile(t, "package main\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {}\n")
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	patch := "--- a/main_test.go\n+++ b/main_test.go\n@@ -5 +5 @@\n-func TestKept(t *testing.T) {}\n+func TestKept(t *testing.T) { t.Skip() }\n"
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Proposal: agents.OptimizerResult{Patch: patch},
	})
	require.NoError(t, err)
	require.Contains(t, evidence.Summary, "candidate rejected before build")
	require.Contains(t, evidence.FailureDetail, `test file "main_test.go" is off-limits`)
	require.Len(t, evidence.ArtifactURIs, 1, "nothing was built")
}
