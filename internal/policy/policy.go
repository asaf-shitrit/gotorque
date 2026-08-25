// Package policy contains the non-LLM acceptance decision for optimization
// candidates. It treats measurements as data and has no
// filesystem, process, or network behavior.
package policy

import (
	"fmt"
	"math"
	"sort"

	"example.com/gotorque/internal/domain"
)

const (
	DefaultMinimumImprovementPercent         = 3.0
	DefaultMaximumGuardrailRegressionPercent = 2.0
)

type Config struct {
	PrimaryMetric                     string
	MinimumImprovementPercent         float64
	MaximumGuardrailRegressionPercent float64
	StatisticalSupportRequired        bool
	Guardrails                        []Guardrail
}

type Guardrail struct {
	Name                     string
	MaximumRegressionPercent float64
	Required                 bool
}

type Evidence struct {
	BehaviorMatches        bool
	SafetyChecksPassed     bool
	RepresentativeEvidence bool
	Comparisons            []Comparison
}

type Comparison struct {
	Name                   string
	Unit                   string
	Baseline               float64
	Candidate              float64
	StatisticallySupported bool
}

type Result struct {
	Decision    domain.Decision
	Comparisons []ComparisonResult
	Reasons     []string
}

type ComparisonResult struct {
	Name                   string
	Unit                   string
	Baseline               float64
	Candidate              float64
	DeltaPercent           float64
	StatisticallySupported bool
}

func DefaultConfig() Config {
	return Config{
		PrimaryMetric:                     "wall_time_ns",
		MinimumImprovementPercent:         DefaultMinimumImprovementPercent,
		MaximumGuardrailRegressionPercent: DefaultMaximumGuardrailRegressionPercent,
		StatisticalSupportRequired:        true,
		Guardrails: []Guardrail{
			{Name: "peak_memory_bytes", MaximumRegressionPercent: DefaultMaximumGuardrailRegressionPercent, Required: true},
			{Name: "cpu_time_ns", MaximumRegressionPercent: DefaultMaximumGuardrailRegressionPercent, Required: true},
			{Name: "binary_size_bytes", MaximumRegressionPercent: DefaultMaximumGuardrailRegressionPercent, Required: true},
		},
	}
}

// Evaluate applies the v1 policy in a stable order. Behavior and safety
// failures are hard rejections. Missing or noisy evidence is inconclusive;
// only a statistically supported representative improvement can be accepted.
func Evaluate(config Config, evidence Evidence) Result {
	config = withDefaults(config)
	result := Result{Decision: domain.DecisionInconclusive}
	result.Comparisons = comparisonResults(evidence.Comparisons)

	if !evidence.BehaviorMatches {
		return reject(result, "behavior does not match the baseline after normalization")
	}
	if !evidence.SafetyChecksPassed {
		return reject(result, "a required safety or validation check failed")
	}
	if !evidence.RepresentativeEvidence {
		return inconclusive(result, "no representative workload evidence is available")
	}

	byName := make(map[string]ComparisonResult, len(result.Comparisons))
	for _, comparison := range result.Comparisons {
		byName[comparison.Name] = comparison
	}
	primary, ok := byName[config.PrimaryMetric]
	if !ok {
		return inconclusive(result, fmt.Sprintf("primary metric %q is missing", config.PrimaryMetric))
	}
	if config.StatisticalSupportRequired && !primary.StatisticallySupported {
		return inconclusive(result, fmt.Sprintf("primary metric %q is not statistically supported", config.PrimaryMetric))
	}
	if !finitePositive(primary.Baseline) || !finite(primary.Candidate) {
		return inconclusive(result, fmt.Sprintf("primary metric %q has invalid measurements", config.PrimaryMetric))
	}

	for _, guardrail := range config.Guardrails {
		comparison, found := byName[guardrail.Name]
		if !found {
			if guardrail.Required {
				return inconclusive(result, fmt.Sprintf("required guardrail %q is missing", guardrail.Name))
			}
			continue
		}
		if !finitePositive(comparison.Baseline) || !finite(comparison.Candidate) {
			return inconclusive(result, fmt.Sprintf("guardrail %q has invalid measurements", guardrail.Name))
		}
		if config.StatisticalSupportRequired && !comparison.StatisticallySupported {
			return inconclusive(result, fmt.Sprintf("guardrail %q is not statistically supported", guardrail.Name))
		}
		if comparison.DeltaPercent > guardrailLimit(config, guardrail) {
			return reject(result, fmt.Sprintf("guardrail %q regressed by %.2f%%, over the %.2f%% limit", guardrail.Name, comparison.DeltaPercent, guardrailLimit(config, guardrail)))
		}
	}

	improvement := -primary.DeltaPercent
	if improvement < config.MinimumImprovementPercent {
		return inconclusive(result, fmt.Sprintf("primary metric improved by %.2f%%, below the %.2f%% threshold", improvement, config.MinimumImprovementPercent))
	}
	result.Decision = domain.DecisionAccepted
	result.Reasons = []string{fmt.Sprintf("primary metric improved by %.2f%% with required evidence and no guardrail regression", improvement)}
	return result
}

func withDefaults(config Config) Config {
	if config.PrimaryMetric == "" {
		config.PrimaryMetric = "wall_time_ns"
	}
	if config.MinimumImprovementPercent == 0 {
		config.MinimumImprovementPercent = DefaultMinimumImprovementPercent
	}
	if config.MaximumGuardrailRegressionPercent == 0 {
		config.MaximumGuardrailRegressionPercent = DefaultMaximumGuardrailRegressionPercent
	}
	if len(config.Guardrails) == 0 {
		config.Guardrails = DefaultConfig().Guardrails
	}
	for i := range config.Guardrails {
		if config.Guardrails[i].MaximumRegressionPercent == 0 {
			config.Guardrails[i].MaximumRegressionPercent = config.MaximumGuardrailRegressionPercent
		}
	}
	return config
}

func comparisonResults(comparisons []Comparison) []ComparisonResult {
	result := make([]ComparisonResult, 0, len(comparisons))
	for _, comparison := range comparisons {
		delta := math.NaN()
		if comparison.Baseline > 0 && finite(comparison.Baseline) && finite(comparison.Candidate) {
			delta = (comparison.Candidate - comparison.Baseline) / comparison.Baseline * 100
		}
		result = append(result, ComparisonResult{
			Name: comparison.Name, Unit: comparison.Unit, Baseline: comparison.Baseline,
			Candidate: comparison.Candidate, DeltaPercent: delta,
			StatisticallySupported: comparison.StatisticallySupported,
		})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func guardrailLimit(config Config, guardrail Guardrail) float64 {
	if guardrail.MaximumRegressionPercent > 0 {
		return guardrail.MaximumRegressionPercent
	}
	return config.MaximumGuardrailRegressionPercent
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func finitePositive(value float64) bool { return value > 0 && finite(value) }

func reject(result Result, reason string) Result {
	result.Decision = domain.DecisionRejected
	result.Reasons = []string{reason}
	return result
}

func inconclusive(result Result, reason string) Result {
	result.Decision = domain.DecisionInconclusive
	result.Reasons = []string{reason}
	return result
}
