// Package policy contains the non-LLM acceptance decision for optimization
// candidates. It treats measurements as data and has no
// filesystem, process, or network behavior.
package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"

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
	BehaviorMatches bool
	// FailureSummary states what actually happened when an evaluation did not
	// reach a behavior comparison: the patch failed to apply, the build failed,
	// or the upstream test suite failed. Without it every such candidate is
	// reported as a behavior mismatch, which misdescribes the rejection to
	// anyone reading a campaign report.
	FailureSummary         string
	SafetyChecksPassed     bool
	RepresentativeEvidence bool
	// Comparisons are the measured comparisons behind a verdict: the pooled
	// reading of every metric plus one per-workload reading of the primary
	// metric. They are the same value type the campaign stores, so nothing has
	// to be translated on the way in or out.
	Comparisons []domain.MetricComparison
	// Primary lists the comparisons a verdict may rest on: every reading of the
	// primary metric — the pooled one (Workload empty) and one per
	// acceptance-eligible workload. The engine measures only representative-tier
	// seeds, so every per-workload entry it supplies is eligible by
	// construction.
	//
	// The set exists because pooling is a poor fit for a seed whose run is
	// dominated by process startup: on gron a candidate improved its
	// large-document seed by 3.35% with support (benchstat p=0.006) while the
	// 41-byte seed, whose measured time is mostly exec and runtime init, sat
	// at -0.81% unsupported and pulled the pooled figure to -2.53% against a
	// 3% bar. An empty set means "judge the pooled primary metric alone".
	Primary []domain.MetricComparison
}

type Result struct {
	Decision    domain.Decision
	Comparisons []domain.MetricComparison
	Reasons     []string
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
// only a statistically supported improvement on an acceptance-eligible
// workload can be accepted.
func Evaluate(config Config, evidence Evidence) Result {
	config = withDefaults(config)
	result := Result{Decision: domain.DecisionInconclusive, Comparisons: comparisonResults(evidence.Comparisons)}
	if early, done := evidenceGates(result, evidence); done {
		return early
	}
	byName := indexComparisons(result.Comparisons)
	eligible, result, ok := eligiblePrimary(config, evidence, result)
	if !ok {
		return result
	}
	if early, done := checkGuardrails(config, byName, result); done {
		return early
	}
	if early, done := checkEligibleRegressions(config, eligible, result); done {
		return early
	}
	result = decideOnImprovement(config, eligible, result)
	for _, comparison := range unconfirmed(config, eligible) {
		result.Reasons = append(result.Reasons, fmt.Sprintf("%s read %+.2f%%, over the %.2f%% limit, but the difference is not statistically significant", readingSubject(comparison), comparison.DeltaPercent, config.MaximumGuardrailRegressionPercent))
	}
	return result
}

// eligiblePrimary resolves the comparisons that may carry acceptance. A caller
// that supplies no per-workload breakdown keeps the pooled-only behavior,
// reasons included.
func eligiblePrimary(config Config, evidence Evidence, result Result) ([]domain.MetricComparison, Result, bool) {
	if len(evidence.Primary) == 0 {
		primary, updated, ok := lookupPrimary(config, indexComparisons(result.Comparisons), result)
		if !ok {
			return nil, updated, false
		}
		return []domain.MetricComparison{primary}, updated, true
	}
	eligible := comparisonResults(evidence.Primary)
	if len(eligible) == 0 {
		return nil, inconclusive(result, fmt.Sprintf("primary metric %q is missing", config.PrimaryMetric)), false
	}
	return eligible, result, true
}

// checkEligibleRegressions refuses a candidate that bought its win on one
// workload by hurting another acceptance-eligible one past the manifest's
// ceiling: the eligible set is a set of representative workloads, not a menu.
//
// A reading over the ceiling rejects only when the difference is significant,
// whenever the manifest asks for statistical support. On the point estimate
// alone, a no-op patch measured against itself on gron was rejected in 4 of 30
// trials: a burst of slow runs moved a 25-pair mean by up to 10% while the
// medians stayed within 1%, and none of those readings was significant. A
// live campaign lost a -20.4% supported win that way to a small-doc reading of
// +4.01% with p above 0.05. The engine measures again before a reading gets
// here unconfirmed (UnconfirmedRegressions), so a real regression the first
// series could not resolve still has more samples to show up in; one that
// stays insignificant is named in the verdict's reasons instead.
func checkEligibleRegressions(config Config, eligible []domain.MetricComparison, result Result) (Result, bool) {
	for _, comparison := range eligible {
		if !finitePositive(comparison.Baseline) || !finite(comparison.Candidate) {
			return inconclusive(result, readingSubject(comparison)+" has invalid measurements"), true
		}
		if overLimit(config, comparison) && (comparison.Significant || !config.StatisticalSupportRequired) {
			return reject(result, fmt.Sprintf("%s regressed by %+.2f%%, over the %.2f%% limit", readingSubject(comparison), comparison.DeltaPercent, config.MaximumGuardrailRegressionPercent)), true
		}
	}
	return result, false
}

// UnconfirmedRegressions returns the eligible readings over the regression
// limit whose difference is not significant: the ones Evaluate will not
// reject on. The engine measures again when there are any, so the rule it
// uses to decide that is the one the verdict applies. It is empty when the
// manifest does not ask for statistical support, since the point estimate
// then decides alone.
func UnconfirmedRegressions(config Config, eligible []domain.MetricComparison) []domain.MetricComparison {
	return unconfirmed(withDefaults(config), comparisonResults(eligible))
}

func unconfirmed(config Config, eligible []domain.MetricComparison) []domain.MetricComparison {
	if !config.StatisticalSupportRequired {
		return nil
	}
	var readings []domain.MetricComparison
	for _, comparison := range eligible {
		if overLimit(config, comparison) && !comparison.Significant {
			readings = append(readings, comparison)
		}
	}
	return readings
}

func overLimit(config Config, comparison domain.MetricComparison) bool {
	return comparison.DeltaPercent > config.MaximumGuardrailRegressionPercent
}

// decideOnImprovement accepts on the best supported win in the eligible set.
// The best unsupported improvement still shapes the reason, so an operator can
// see how close an unsupported candidate came.
func decideOnImprovement(config Config, eligible []domain.MetricComparison, result Result) Result {
	best, supported := bestImprovement(eligible)
	if !supported {
		return inconclusive(result, fmt.Sprintf("no acceptance-eligible measurement has a statistically supported %q improvement (best %.2f%%)", config.PrimaryMetric, -best.DeltaPercent))
	}
	if -best.DeltaPercent < config.MinimumImprovementPercent {
		return inconclusive(result, fmt.Sprintf("best acceptance-eligible measurement improved by %.2f%%, below the %.2f%% threshold", -best.DeltaPercent, config.MinimumImprovementPercent))
	}
	result.Decision = domain.DecisionAccepted
	result.Reasons = []string{fmt.Sprintf("%s improved by %.2f%% with required evidence and no guardrail regression", acceptanceSubject(best), -best.DeltaPercent)}
	return result
}

// bestImprovement returns the largest improvement in the eligible set together
// with whether that comparison carried statistical support. The supported
// comparison wins when one exists, because that is the one acceptance rests
// on.
func bestImprovement(eligible []domain.MetricComparison) (domain.MetricComparison, bool) {
	best := eligible[0]
	supported := best.StatisticallyFit
	for _, comparison := range eligible[1:] {
		better := comparison.DeltaPercent < best.DeltaPercent
		if better || (comparison.StatisticallyFit && !supported) {
			best, supported = comparison, comparison.StatisticallyFit
		}
	}
	return best, supported
}

// acceptanceSubject names what a verdict rests on: the pooled metric keeps its
// original wording, a per-workload win names the workload by its manifest seed
// id — the name the operator writes in the target manifest — rather than the
// derived run identifier the comparison is keyed by internally.
func acceptanceSubject(comparison domain.MetricComparison) string {
	if comparison.Workload == "" {
		return "primary metric"
	}
	return fmt.Sprintf("workload %q", comparison.Workload)
}

// readingSubject names one comparison inside a policy sentence: the workload
// when the reading is per-workload, the pooled metric when it is the
// aggregate. Falling back to the bare metric name in the workload's slot
// produced `workload "wall_time_ns" regressed by +2.28%` in a live campaign.
func readingSubject(comparison domain.MetricComparison) string {
	if comparison.Workload != "" {
		return fmt.Sprintf("workload %q", comparison.Workload)
	}
	return "the pooled " + comparison.Metric
}

func evidenceGates(result Result, evidence Evidence) (Result, bool) {
	if !evidence.BehaviorMatches {
		if summary := strings.TrimSpace(evidence.FailureSummary); summary != "" {
			return reject(result, summary), true
		}
		return reject(result, "behavior does not match the baseline after normalization"), true
	}
	if !evidence.SafetyChecksPassed {
		return reject(result, "a required safety or validation check failed"), true
	}
	if !evidence.RepresentativeEvidence {
		return inconclusive(result, "no representative workload evidence is available"), true
	}
	return result, false
}

// indexComparisons keys the pooled readings by metric name. Only pooled
// entries participate: the primary metric and the guardrails are aggregates
// over every eligible workload, while per-workload readings are judged
// through the eligible set supplied as Evidence.Primary.
func indexComparisons(comparisons []domain.MetricComparison) map[string]domain.MetricComparison {
	byName := make(map[string]domain.MetricComparison, len(comparisons))
	for _, comparison := range comparisons {
		if comparison.Workload != "" {
			continue
		}
		byName[comparison.Metric] = comparison
	}
	return byName
}

func lookupPrimary(config Config, byName map[string]domain.MetricComparison, result Result) (domain.MetricComparison, Result, bool) {
	primary, ok := byName[config.PrimaryMetric]
	if !ok {
		return primary, inconclusive(result, fmt.Sprintf("primary metric %q is missing", config.PrimaryMetric)), false
	}
	if config.StatisticalSupportRequired && !primary.StatisticallyFit {
		return primary, inconclusive(result, fmt.Sprintf("primary metric %q is not statistically supported", config.PrimaryMetric)), false
	}
	if !finitePositive(primary.Baseline) || !finite(primary.Candidate) {
		return primary, inconclusive(result, fmt.Sprintf("primary metric %q has invalid measurements", config.PrimaryMetric)), false
	}
	return primary, result, true
}

func checkGuardrails(config Config, byName map[string]domain.MetricComparison, result Result) (Result, bool) {
	for _, guardrail := range config.Guardrails {
		if early, done := checkOneGuardrail(config, guardrail, byName, result); done {
			return early, true
		}
	}
	return result, false
}

func checkOneGuardrail(config Config, guardrail Guardrail, byName map[string]domain.MetricComparison, result Result) (Result, bool) {
	comparison, found := byName[guardrail.Name]
	if !found {
		if guardrail.Required {
			return inconclusive(result, fmt.Sprintf("required guardrail %q is missing", guardrail.Name)), true
		}
		return result, false
	}
	if !finitePositive(comparison.Baseline) || !finite(comparison.Candidate) {
		return inconclusive(result, fmt.Sprintf("guardrail %q has invalid measurements", guardrail.Name)), true
	}
	// The manifest's guardrail contract is a threshold: reject when the
	// guardrail regressed past maximum_regression_percent. Requiring
	// statistical support here as well asked a required guardrail to prove
	// the absence of a regression, which a jittery metric cannot do at seven
	// samples. A measured +0.16% against the 2% limit was reported
	// inconclusive and blocked a candidate whose primary metric improved
	// 14.38% with support. StatisticalSupportRequired still guards the
	// primary metric, where it protects the win itself.
	if comparison.DeltaPercent > guardrailLimit(config, guardrail) {
		return reject(result, fmt.Sprintf("guardrail %q regressed by %.2f%%, over the %.2f%% limit", guardrail.Name, comparison.DeltaPercent, guardrailLimit(config, guardrail))), true
	}
	return result, false
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

func comparisonResults(comparisons []domain.MetricComparison) []domain.MetricComparison {
	result := make([]domain.MetricComparison, 0, len(comparisons))
	for _, comparison := range comparisons {
		delta := math.NaN()
		if comparison.Baseline > 0 && finite(comparison.Baseline) && finite(comparison.Candidate) {
			delta = (comparison.Candidate - comparison.Baseline) / comparison.Baseline * 100
		}
		comparison.DeltaPercent = delta
		result = append(result, comparison)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Metric != result[j].Metric {
			return result[i].Metric < result[j].Metric
		}
		return result[i].Workload < result[j].Workload
	})
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
