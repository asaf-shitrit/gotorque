package policy

import (
	"math"
	"testing"

	"example.com/gotorque/internal/domain"
)

func TestEvaluateAccepted(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 95, StatisticallySupported: true},
			{Name: "peak_memory_bytes", Baseline: 100, Candidate: 101, StatisticallySupported: true},
			{Name: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallySupported: true},
			{Name: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
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
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 90, StatisticallySupported: true},
			{Name: "peak_memory_bytes", Baseline: 100, Candidate: 103, StatisticallySupported: true},
			{Name: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallySupported: true},
			{Name: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
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
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 85.62, StatisticallySupported: true},
			{Name: "peak_memory_bytes", Baseline: 14811000, Candidate: 14835000, StatisticallySupported: false},
			{Name: "cpu_time_ns", Baseline: 100, Candidate: 84.37, StatisticallySupported: true},
			{Name: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
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
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 85, StatisticallySupported: true},
			{Name: "peak_memory_bytes", Baseline: 100, Candidate: 106, StatisticallySupported: false},
			{Name: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallySupported: true},
			{Name: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
		},
	})
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

func TestEvaluateInconclusiveBelowThreshold(t *testing.T) {
	result := Evaluate(DefaultConfig(), Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 98, StatisticallySupported: true},
			{Name: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
			{Name: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallySupported: true},
			{Name: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallySupported: true},
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
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: 100, Candidate: 90, StatisticallySupported: false},
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
		Comparisons: []Comparison{
			{Name: "wall_time_ns", Baseline: math.NaN(), Candidate: 1, StatisticallySupported: true},
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
