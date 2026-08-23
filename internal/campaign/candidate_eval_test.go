package campaign

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/gotorque/internal/domain"
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

func TestCompareMetric(t *testing.T) {
	mk := func(name string, vals ...float64) []domain.RunResult {
		runs := make([]domain.RunResult, 0, len(vals))
		for _, v := range vals {
			runs = append(runs, runWithMetric(name, v))
		}
		return runs
	}

	t.Run("missing baseline metric yields bare comparison", func(t *testing.T) {
		got := compareMetric("wl", "wall_time_ns", "ns", []domain.RunResult{{}}, mk("wall_time_ns", 1, 2), wallTime)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		c := got[0]
		if c.Name != "wl/wall_time_ns" || c.Unit != "ns" || c.Baseline != 0 || c.Candidate != 0 || c.DeltaPercent != 0 || c.StatisticallyFit {
			t.Fatalf("unexpected comparison %+v", c)
		}
	})

	t.Run("zero-mean baseline has no percent delta but may be supported", func(t *testing.T) {
		base := mk("peak_memory_bytes", 0, 0, 0, 0)
		// constant zeros: se2==0, means equal -> not supported; DeltaPercent stays 0.
		cand := mk("peak_memory_bytes", 0, 0, 0, 0)
		got := compareMetric("w", "peak_memory_bytes", "bytes", base, cand, peakMemory)
		c := got[0]
		if c.Baseline != 0 || c.Candidate != 0 || c.DeltaPercent != 0 || c.StatisticallyFit {
			t.Fatalf("unexpected comparison %+v", c)
		}
	})

	t.Run("clear improvement is statistically fit with correct delta", func(t *testing.T) {
		base := mk("wall_time_ns", 100, 102, 99, 101, 100, 98, 101)
		cand := mk("wall_time_ns", 50, 51, 49, 50, 52, 48, 51)
		got := compareMetric("wid", "wall_time_ns", "ns", base, cand, wallTime)
		if len(got) != 1 {
			t.Fatalf("len = %d", len(got))
		}
		c := got[0]
		if c.Name != "wid/wall_time_ns" || c.Unit != "ns" {
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
	})

	t.Run("noise not marked fit", func(t *testing.T) {
		base := mk("cpu_time_ns", 100, 105, 95, 103, 97, 102, 98)
		cand := mk("cpu_time_ns", 101, 96, 104, 99, 102, 97, 100)
		got := compareMetric("w", "cpu_time_ns", "ns", base, cand, cpuTime)[0]
		if got.StatisticallyFit {
			t.Fatalf("noise should not be statistically fit: %+v", got)
		}
	})
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
