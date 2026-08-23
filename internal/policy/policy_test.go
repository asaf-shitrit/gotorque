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
