package campaign

import (
	"math"
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/domain"
)

// shiftFromSamples fills in the outlier-robust shift of one comparison from
// the raw baseline and candidate samples behind it. The mean delta stays in
// DeltaPercent for the regression and guardrail checks; the shift is what
// acceptance thresholds on (ADR 0040).
func shiftFromSamples(comparison *domain.MetricComparison, baseline, candidate []float64) {
	percent, ok := robustShiftPercent(baseline, candidate)
	if !ok {
		return
	}
	comparison.Shift = domain.ShiftHodgesLehmann
	comparison.ShiftPercent = percent
}

// robustShiftPercent expresses the Hodges–Lehmann shift as a percentage of the
// baseline median, the robust measure of the baseline's location. It reports
// false when there are too few samples to estimate a shift or when the
// baseline median is not positive, so a caller falls back to the mean delta.
func robustShiftPercent(baseline, candidate []float64) (float64, bool) {
	location, ok := median(baseline)
	if !ok || location <= 0 {
		return 0, false
	}
	shift, ok := hodgesLehmannShift(baseline, candidate)
	if !ok {
		return 0, false
	}
	return shift / location * 100, true
}

// hodgesLehmannShift returns the Hodges–Lehmann estimate of the shift from
// baseline to candidate: the median of every candidate-minus-baseline pairwise
// difference. The samples are treated as independent, which is the assumption
// the two-sample comparison behind a verdict already makes, and that is why
// this estimator goes with it. Unlike the mean, a few slow runs on one side
// move it only as far as they move the middle pair.
func hodgesLehmannShift(baseline, candidate []float64) (float64, bool) {
	base := usableSamples(baseline)
	cand := usableSamples(candidate)
	if len(base) < 2 || len(cand) < 2 {
		return 0, false
	}
	differences := make([]float64, 0, len(base)*len(cand))
	for _, c := range cand {
		for _, b := range base {
			differences = append(differences, c-b)
		}
	}
	shift, ok := median(differences)
	if !ok {
		return 0, false
	}
	return shift, true
}

// median returns the middle value of values, averaging the two middle values
// for an even count. It copies before sorting, so the caller's order survives.
func median(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle], true
	}
	return (sorted[middle-1] + sorted[middle]) / 2, true
}

// usableSamples drops the readings a mean would have been refused for: a NaN
// or an infinity. A comparison only reaches the shift when its samples are
// finite, but a replay reads them back from a recorded campaign, so the
// defensive filter stays here rather than in the caller.
func usableSamples(values []float64) []float64 {
	usable := make([]float64, 0, len(values))
	for _, value := range values {
		if !math.IsNaN(value) && !math.IsInf(value, 0) {
			usable = append(usable, value)
		}
	}
	return usable
}
