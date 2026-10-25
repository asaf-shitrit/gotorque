package campaign

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
)

func TestHodgesLehmannShiftIsTheMedianPairwiseDifference(t *testing.T) {
	shift, ok := hodgesLehmannShift([]float64{10, 20, 30}, []float64{11, 21, 31})
	require.True(t, ok)
	require.InDelta(t, 1, shift, 1e-9)

	// The candidate is uniformly 10% faster, so the estimate is the shift the
	// samples were built with, not the mean difference.
	baseline := []float64{100, 110, 120, 90, 105}
	candidate := []float64{90, 99, 108, 81, 94.5}
	shift, ok = hodgesLehmannShift(baseline, candidate)
	require.True(t, ok)
	require.InDelta(t, -0.1*medianOf(t, baseline), shift, 1e-9)
}

// TestHodgesLehmannShiftIgnoresSlowRunBursts is the property the robust shift
// exists for: two slow baseline runs inflated go-jsonnet's order-report mean
// from -2.56% (Hodges–Lehmann) to -5.16%, which accepted a candidate the
// typical shift put below the minimum.
func TestHodgesLehmannShiftIgnoresSlowRunBursts(t *testing.T) {
	baseline := []float64{54, 55, 56, 56, 57, 57, 58, 59, 59, 60, 60, 61, 63, 64, 85, 86}
	candidate := []float64{54, 55, 55, 56, 56, 56, 57, 57, 58, 59, 60, 61, 66, 68}
	location := medianOf(t, baseline)
	meanShift := (meanOf(candidate) - meanOf(baseline)) / location * 100
	robust, ok := robustShiftPercent(baseline, candidate)
	require.True(t, ok)
	require.Less(t, meanShift, -5.0, "the mean is inflated by the two slow baseline runs")
	require.Greater(t, robust, -3.0, "the robust shift keeps the typical effect under the minimum")
}

func TestRobustShiftPercentIsRelativeToTheBaselineMedian(t *testing.T) {
	percent, ok := robustShiftPercent([]float64{100, 100, 100}, []float64{90, 90, 90})
	require.True(t, ok)
	require.InDelta(t, -10, percent, 1e-9)

	// A skewed baseline moves the mean but not the median, so the percentage
	// does not move either.
	percent, ok = robustShiftPercent([]float64{100, 100, 100, 100, 400}, []float64{90, 90, 90, 90, 90})
	require.True(t, ok)
	require.InDelta(t, -10, percent, 1e-9)
}

func TestRobustShiftPercentRefusesUnusableSamples(t *testing.T) {
	for name, tc := range map[string]struct {
		baseline  []float64
		candidate []float64
	}{
		"no baseline":         {nil, []float64{1, 2}},
		"one baseline":        {[]float64{1}, []float64{1, 2}},
		"one candidate":       {[]float64{1, 2}, []float64{1}},
		"zero baseline":       {[]float64{0, 0}, []float64{1, 2}},
		"negative baseline":   {[]float64{-1, -2}, []float64{1, 2}},
		"non-finite baseline": {[]float64{100, math.Inf(1)}, []float64{100, 90}},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := robustShiftPercent(tc.baseline, tc.candidate)
			require.False(t, ok)
		})
	}
}

func TestShiftFromSamplesFillsTheComparison(t *testing.T) {
	comparison := domain.MetricComparison{Metric: "wall_time_ns", Baseline: 100, DeltaPercent: -5}
	shiftFromSamples(&comparison, []float64{100, 100, 100}, []float64{90, 90, 90})
	require.Equal(t, domain.ShiftHodgesLehmann, comparison.Shift)
	require.InDelta(t, -10, comparison.ShiftPercent, 1e-9)
	require.InDelta(t, -5, comparison.DeltaPercent, 1e-9, "the mean delta is left alone")

	unusable := domain.MetricComparison{Metric: "binary_size_bytes"}
	shiftFromSamples(&unusable, nil, nil)
	require.Empty(t, unusable.Shift)
}

func medianOf(t *testing.T, values []float64) float64 {
	t.Helper()
	value, ok := median(values)
	require.True(t, ok)
	return value
}

func meanOf(values []float64) float64 {
	value, _ := mean(values)
	return value
}
