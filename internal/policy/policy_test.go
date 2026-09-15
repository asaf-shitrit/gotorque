package policy

import (
	"math"
	"strings"
	"testing"

	"example.com/gotorque/internal/domain"
)

func TestEvaluateAccepted(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 95, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 101, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

func TestEvaluateRejectsBehaviorChangeBeforePerformance(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{BehaviorMatches: false, SafetyChecksPassed: true, RepresentativeEvidence: true})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s", result.Decision)
	}
}

func TestEvaluateRejectsGuardrailRegression(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 90, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 103, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

// TestEvaluateAcceptsWithinLimitUnsupportedGuardrail pins the manifest's
// guardrail contract: the limit is the barrier, not proof of the absence of a
// regression. A required guardrail that moved +0.16% against a 2% limit used
// to be reported inconclusive and blocked a candidate whose primary metric
// improved 14.38% with statistical support, because a jittery high-water mark
// cannot be shown flat at seven samples.
func TestEvaluateAcceptsWithinLimitUnsupportedGuardrail(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 85.62, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 14811000, Candidate: 14835000, StatisticallyFit: false},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 84.37, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

// TestEvaluateRejectsUnsupportedGuardrailOverLimit keeps the threshold binding
// even when the guardrail measurement carries no statistical support.
func TestEvaluateRejectsUnsupportedGuardrailOverLimit(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 85, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 106, StatisticallyFit: false},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

func TestEvaluateInconclusiveBelowThreshold(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionInconclusive {
		t.Fatalf("decision = %s", result.Decision)
	}
}

func TestEvaluateInconclusiveWithoutRepresentativeEvidence(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{BehaviorMatches: true, SafetyChecksPassed: true})
	if result.Decision != domain.DecisionInconclusive {
		t.Fatalf("decision = %s", result.Decision)
	}
}

func TestEvaluateInconclusiveWithoutStatisticalSupport(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 90, StatisticallyFit: false},
		},
	})
	if result.Decision != domain.DecisionInconclusive {
		t.Fatalf("decision = %s", result.Decision)
	}
}

func TestEvaluateRejectsFailedSafety(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{BehaviorMatches: true, SafetyChecksPassed: false, RepresentativeEvidence: true})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s", result.Decision)
	}
}

func TestComparisonResultsAreSortedAndInvalidValuesAreInconclusive(t *testing.T) {
	result := Evaluate(Config{PrimaryMetric: "wall_time_ns", Guardrails: []Guardrail{}}, Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: math.NaN(), Candidate: 1, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionInconclusive || len(result.Comparisons) != 1 {
		t.Fatalf("decision = %s, comparisons = %d", result.Decision, len(result.Comparisons))
	}
}

func TestEvaluateReportsWhyBehaviorWasNeverVerified(t *testing.T) {
	evidence := Evidence{
		FailureSummary:         "candidate rejected before build: git apply check failed",
		SafetyChecksPassed:     true,
		RepresentativeEvidence: true,
	}
	result := Evaluate(DefaultConfig(), evidence)
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("Decision = %v, want rejected", result.Decision)
	}
	want := "candidate rejected before build: git apply check failed"
	if len(result.Reasons) != 1 || result.Reasons[0] != want {
		t.Errorf("Reasons = %v, want [%q]", result.Reasons, want)
	}
}

func TestEvaluateFallsBackToBehaviorMismatchReason(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{SafetyChecksPassed: true, RepresentativeEvidence: true})
	want := "behavior does not match the baseline after normalization"
	if len(result.Reasons) != 1 || result.Reasons[0] != want {
		t.Errorf("Reasons = %v, want [%q]", result.Reasons, want)
	}
}

// A seed whose run is dominated by process startup cannot be improved by any
// patch, so it must not be able to veto a supported win elsewhere: gron's
// large-document seed improved 3.35% with support (benchstat p=0.006) while
// the 41-byte seed stayed flat and pulled the pooled figure to 2.53%, under a
// 3% bar. The eligible set is where that decision lives.
func TestEvaluateAcceptsOnOneEligibleWorkload(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 97.47, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 39.68, StatisticallyFit: false},
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57.99, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 98, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 97.47, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 39.68, StatisticallyFit: false},
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57.99, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if len(result.Reasons) == 0 || !strings.Contains(result.Reasons[0], `workload "big"`) {
		t.Fatalf("reasons should name the workload the verdict rests on: %v", result.Reasons)
	}
}

func TestEvaluateRefusesWhenNoEligibleWorkloadClearsTheThreshold(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 39.8, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 58.56, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 58.56, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionInconclusive {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if !strings.Contains(result.Reasons[0], "below the 3.00% threshold") {
		t.Fatalf("reason = %v", result.Reasons)
	}
}

// A candidate that wins on one workload by hurting another representative one
// is not a win: the eligible set is a set, not a menu.
func TestEvaluateRejectsRegressionOnAnotherEligibleWorkload(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 41, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 41, StatisticallyFit: true},
		},
	})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if !strings.Contains(result.Reasons[0], "over the 2.00% limit") {
		t.Fatalf("reason = %v", result.Reasons)
	}
}

// A large movement nobody can attribute is not evidence, so an unsupported
// workload win cannot carry acceptance while a supported small one exists.
func TestEvaluateWillNotAcceptAnUnsupportedWorkloadWin(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 99.7, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 56.4, StatisticallyFit: false},
		},
	})
	if result.Decision != domain.DecisionInconclusive {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if !strings.Contains(result.Reasons[0], "statistically supported") {
		t.Fatalf("reason = %v", result.Reasons)
	}
}
