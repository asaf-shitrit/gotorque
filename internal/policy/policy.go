// Package policy contains the non-LLM acceptance decision for optimization
// candidates. It treats measurements as data and has no
// filesystem, process, or network behavior.
package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/domain"
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
	// acceptance-eligible workload. The engine measures only the workloads whose
	// tier the manifest makes acceptance-eligible, so every per-workload entry it
	// supplies is eligible by construction.
	//
	// The set exists because pooling is a poor fit for a seed whose run is
	// dominated by process startup: on gron a candidate improved its
	// large-document seed by 3.35% with support (benchstat p=0.006) while the
	// 41-byte seed, whose measured time is mostly exec and runtime init, sat
	// at -0.81% unsupported and pulled the pooled figure to -2.53% against a
	// 3% bar. An empty set means "judge the pooled primary metric alone".
	Primary []domain.MetricComparison
	// StressComparisons are the guardrail readings of workloads the manifest
	// keeps out of acceptance: its plausible and stress tiers. They can only
	// veto, never carry, which is what the target manifest has always promised
	// a stress seed does (ADR 0046). An entry naming a metric that is not a
	// configured guardrail is ignored, so a caller cannot slip a wall-time win
	// from an ineligible workload into a verdict.
	StressComparisons []domain.MetricComparison
	// FreshGuardrails are the guardrail readings measured on a fresh series of
	// pairs, taken after the first one. A guardrail a single series can
	// misreport (replicatedGuardrail) rejects only when this reading
	// reproduces the breach; the engine runs that series when
	// GuardrailsToReplicate or StressGuardrailsToReplicate names a breach, or
	// when another reading asks for a confirmation series, and records the
	// readings here for every such series. A pooled reading carries no
	// workload; a stress seed's carries its id, because the series that checks
	// one of those breaches runs on that seed alone.
	FreshGuardrails []domain.MetricComparison
	// FirstSeriesGuardrails are the pooled guardrail readings of the first
	// series, for the guardrails a fresh series re-measures
	// (ReplicatedGuardrails), taken before that series was folded in. They are
	// recorded because Comparisons then hold one pooled value for both series:
	// a reason that says what the first series read cannot quote that value.
	// An empty set means no fresh series ran, so Comparisons still hold the
	// first series' own readings.
	FirstSeriesGuardrails []domain.MetricComparison
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
	if early, done := checkGuardrails(config, byName, evidence, result); done {
		return early
	}
	if early, done := checkEligibleRegressions(config, eligible, result); done {
		return early
	}
	if early, done := checkStressComparisons(config, evidence, result); done {
		return early
	}
	result = decideOnImprovement(config, eligible, result)
	return noteUnactedReadings(config, evidence, byName, eligible, result)
}

// noteUnactedReadings appends the reasons for every reading that was over its
// limit and did not reject: an insignificant regression, an insignificant
// pooled guardrail, an insignificant stress reading, and a breach a fresh
// series did not reproduce. None of them may hide the verdict's improvement,
// so they are named rather than acted on (ADR 0016, ADR 0040).
func noteUnactedReadings(config Config, evidence Evidence, byName map[string]domain.MetricComparison, eligible []domain.MetricComparison, result Result) Result {
	for _, comparison := range unconfirmed(config, eligible) {
		result.Reasons = append(result.Reasons, fmt.Sprintf("%s read %+.2f%%, over the %.2f%% limit, but the difference is not statistically significant", readingSubject(comparison), comparison.DeltaPercent, config.MaximumGuardrailRegressionPercent))
	}
	for _, g := range unconfirmedGuardrails(config, byName) {
		result.Reasons = append(result.Reasons, fmt.Sprintf("guardrail %q read %+.2f%%, over the %.2f%% limit, but the difference is not statistically significant", g.name, g.comparison.DeltaPercent, g.limit))
	}
	for _, reading := range unconfirmedStress(config, evidence.StressComparisons) {
		result.Reasons = append(result.Reasons, fmt.Sprintf("guardrail %q read %+.2f%% on workload %q, over the %.2f%% limit, but the difference is not statistically significant; that workload cannot carry a verdict", reading.name, reading.comparison.DeltaPercent, reading.comparison.Workload, reading.limit))
	}
	for _, reading := range unreplicatedStress(config, evidence) {
		result.Reasons = append(result.Reasons, unreplicatedReason(reading))
	}
	for _, g := range unreplicatedGuardrails(config, byName, evidence) {
		result.Reasons = append(result.Reasons, unreplicatedReason(g))
	}
	return result
}

// checkStressComparisons refuses a candidate that regressed a configured
// guardrail on a workload the manifest keeps out of acceptance. The workloads
// are measured, but their wall time is never compared: a seed that cannot
// carry an acceptance must not be able to argue for one, while its behavior
// (judged before this policy runs) and its guardrails can still reject.
//
// The rule is the one the representative guardrails use: over the limit and
// significant, or over the limit alone when the manifest does not ask for
// statistical support. Peak memory is an exception on these seeds for the same
// reason it is on the pooled readings: it is a per-run high-water mark, so one
// series can read it high without the candidate costing more memory, and a
// stress seed's only power is to veto. Such a breach is decided by a fresh
// series of the same seed (decideReplicatedBreach, ADR 0040). These readings
// get no other confirmation series, so an insignificant one is named in the
// reasons instead of rejecting (unconfirmedStress), the way an insignificant
// pooled guardrail is.
func checkStressComparisons(config Config, evidence Evidence, result Result) (Result, bool) {
	for _, comparison := range comparisonResults(evidence.StressComparisons) {
		guardrail, ok := guardrailNamed(config, comparison.Metric)
		if !ok || !finitePositive(comparison.Baseline) || !finite(comparison.Candidate) {
			continue
		}
		limit := guardrailLimit(config, guardrail)
		if replicatedGuardrail(guardrail.Name) && config.StatisticalSupportRequired {
			if early, done := checkReplicatedStressGuardrail(config, comparison, limit, evidence, result); done {
				return early, true
			}
			continue
		}
		if breached(config, comparison, limit) {
			return reject(result, stressBreachReason(comparison, limit)+stressBreachNote), true
		}
	}
	return result, false
}

// checkReplicatedStressGuardrail decides one stress seed's peak-memory breach
// from the fresh series measured on that seed, by the rule ADR 0040 sets for
// every peak-memory breach.
func checkReplicatedStressGuardrail(config Config, breach domain.MetricComparison, limit float64, evidence Evidence, result Result) (Result, bool) {
	if !breached(config, breach, limit) {
		return result, false
	}
	fresh, measured := freshStressGuardrail(evidence, breach)
	return decideReplicatedBreach(config, replicatedBreach{
		name: breach.Metric, workload: breach.Workload, limit: limit,
		reading: breach, fresh: fresh, measured: measured,
	}, result)
}

// stressBreachReason names a guardrail reading on a workload the manifest
// keeps out of acceptance, and stressBreachNote closes the reason with why a
// rejection may rest on it at all.
func stressBreachReason(comparison domain.MetricComparison, limit float64) string {
	return fmt.Sprintf("guardrail %q regressed by %.2f%% on workload %q, over the %.2f%% limit", comparison.Metric, comparison.DeltaPercent, comparison.Workload, limit)
}

const stressBreachNote = "; the manifest keeps that workload out of acceptance, so it can veto a candidate but never carry one"

// unreplicatedStress returns the stress-workload peak-memory breaches a fresh
// series did not reproduce, for the verdict's reasons.
func unreplicatedStress(config Config, evidence Evidence) []unreplicatedBreach {
	if !config.StatisticalSupportRequired {
		return nil
	}
	var out []unreplicatedBreach
	for _, comparison := range comparisonResults(evidence.StressComparisons) {
		guardrail, ok := guardrailNamed(config, comparison.Metric)
		if !ok || !replicatedGuardrail(guardrail.Name) {
			continue
		}
		limit := guardrailLimit(config, guardrail)
		if !breached(config, comparison, limit) {
			continue
		}
		fresh, measured := freshStressGuardrail(evidence, comparison)
		if !measured || breached(config, fresh, limit) {
			continue
		}
		out = append(out, unreplicatedBreach{name: guardrail.Name, limit: limit, comparison: comparison, fresh: fresh})
	}
	return out
}

// freshStressGuardrail finds the fresh series' own reading of one stress seed's
// guardrail, and whether one was measured. The reading is keyed by workload as
// well as metric: the pooled readings a confirmation series produces carry no
// workload, and both kinds live in Evidence.FreshGuardrails.
func freshStressGuardrail(evidence Evidence, breach domain.MetricComparison) (domain.MetricComparison, bool) {
	for _, comparison := range comparisonResults(evidence.FreshGuardrails) {
		if comparison.Metric == breach.Metric && comparison.Workload == breach.Workload {
			return comparison, true
		}
	}
	return domain.MetricComparison{}, false
}

// unconfirmedStress returns the stress-workload guardrail readings over their
// limit that are not statistically significant: the ones Evaluate names in the
// reasons instead of rejecting on. An insignificant reading is not evidence of
// a regression, and these workloads have no confirmation series to turn one
// into evidence; naming it keeps the reading auditable without letting noise
// reject a good candidate, the rule ADR 0016 set for every other reading. The
// reading carries the limit it was judged by, so the reason states the bound
// the check applied: an --allow or a manifest's own ceiling, not the shared one.
func unconfirmedStress(config Config, comparisons []domain.MetricComparison) []guardrailReading {
	var readings []guardrailReading
	for _, comparison := range comparisonResults(comparisons) {
		guardrail, ok := guardrailNamed(config, comparison.Metric)
		if !ok || !finitePositive(comparison.Baseline) || !finite(comparison.Candidate) {
			continue
		}
		limit := guardrailLimit(config, guardrail)
		if config.StatisticalSupportRequired && comparison.DeltaPercent > limit && !comparison.Significant {
			readings = append(readings, guardrailReading{name: guardrail.Name, limit: limit, comparison: comparison})
		}
	}
	return readings
}

// guardrailNamed resolves a metric name against the configured guardrails.
func guardrailNamed(config Config, name string) (Guardrail, bool) {
	for _, guardrail := range config.Guardrails {
		if guardrail.Name == name {
			return guardrail, true
		}
	}
	return Guardrail{}, false
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

// UnconfirmedGuardrails returns the pooled guardrail readings over their
// limit whose difference is not significant: the ones Evaluate will not
// reject on. The engine measures again when there are any, as it does for
// UnconfirmedRegressions.
func UnconfirmedGuardrails(config Config, comparisons []domain.MetricComparison) []domain.MetricComparison {
	readings := unconfirmedGuardrails(withDefaults(config), indexComparisons(comparisonResults(comparisons)))
	out := make([]domain.MetricComparison, 0, len(readings))
	for _, g := range readings {
		out = append(out, g.comparison)
	}
	return out
}

type guardrailReading struct {
	name       string
	limit      float64
	comparison domain.MetricComparison
}

func unconfirmedGuardrails(config Config, byName map[string]domain.MetricComparison) []guardrailReading {
	if !config.StatisticalSupportRequired {
		return nil
	}
	var readings []guardrailReading
	for _, guardrail := range config.Guardrails {
		comparison, found := byName[guardrail.Name]
		limit := guardrailLimit(config, guardrail)
		if found && comparison.DeltaPercent > limit && !comparison.Significant {
			readings = append(readings, guardrailReading{name: guardrail.Name, limit: limit, comparison: comparison})
		}
	}
	return readings
}

func overLimit(config Config, comparison domain.MetricComparison) bool {
	return comparison.DeltaPercent > config.MaximumGuardrailRegressionPercent
}

// UnconfirmedImprovements returns the eligible readings that improved by at
// least MinimumImprovementPercent but are not statistically supported: the
// ones that would have accepted the candidate had they carried support, and
// instead leave it inconclusive (decideOnImprovement). The engine measures
// again when there are any (ADR 0021), mirroring UnconfirmedRegressions on
// the acceptance side: a real win twenty-five pairs could not resolve still
// has more samples to show up in, and one that stays unsupported is named in
// the verdict's reasons instead. It is empty when the manifest does not ask
// for statistical support, since the point estimate then decides alone.
func UnconfirmedImprovements(config Config, eligible []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	if !config.StatisticalSupportRequired {
		return nil
	}
	var readings []domain.MetricComparison
	for _, comparison := range comparisonResults(eligible) {
		if improvedAtLeastMinimum(config, comparison) && !comparison.StatisticallyFit {
			readings = append(readings, comparison)
		}
	}
	return readings
}

// ImprovementDelta returns the delta acceptance thresholds on: the
// Hodges–Lehmann shift when the engine measured one, the sample-mean delta
// otherwise. The regression and guardrail comparisons deliberately keep
// DeltaPercent (ADR 0043): replaying the recorded campaigns showed a robust
// regression reading turning six A/A null candidates from inconclusive into
// rejected, while this asymmetric rule changed one verdict in the recorded
// replay (306 measured candidates) and only made an accept stricter. A
// comparison with a measured shift whose percent is not finite falls back to
// the mean rather than accepting on a missing number.
func ImprovementDelta(comparison domain.MetricComparison) float64 {
	if hasRobustShift(comparison) {
		return comparison.ShiftPercent
	}
	return comparison.DeltaPercent
}

func hasRobustShift(comparison domain.MetricComparison) bool {
	return comparison.Shift == domain.ShiftHodgesLehmann && finite(comparison.ShiftPercent)
}

// estimatorsAgree reports whether the mean delta and the robust shift point the
// same way. A shift may supply a win's magnitude but never its existence:
// metricSupport (internal/campaign) sets StatisticallyFit for a reading that is
// not an improvement whenever the data confidently rules out a material
// regression, so support there says "no regression", not "faster". Reading a
// win off the shift while the mean says otherwise therefore has no evidence
// behind it, and accepting on it is a false accept: a reading of +1.5% by the
// mean beside a -4% shift was accepted as a 4.00% improvement (ADR 0043). A
// comparison with no measured shift carries only the mean, so it agrees by
// construction. A delta of exactly 0 points no way, so it agrees with neither a
// win nor a loss: a flat mean beside a -4% shift was still accepted as a 4.00%
// improvement while the comparison read `<= 0` as improving.
func estimatorsAgree(comparison domain.MetricComparison) bool {
	if !hasRobustShift(comparison) {
		return true
	}
	if !finite(comparison.DeltaPercent) {
		return false
	}
	return deltaDirection(comparison.DeltaPercent) == deltaDirection(comparison.ShiftPercent)
}

// deltaDirection reports which way a delta points: -1 for an improvement, +1
// for a regression, 0 for a tie.
func deltaDirection(delta float64) int {
	switch {
	case delta < 0:
		return -1
	case delta > 0:
		return 1
	default:
		return 0
	}
}

// BorderlineImprovements returns the reading an accept would rest on when it
// cleared the minimum by less than borderlineFactor, or nothing. The engine
// measures such a candidate again before the verdict, as it does an
// unsupported improvement (UnconfirmedImprovements). A win just past the
// threshold is where an overestimate turns into an accept: held-out dyff was
// accepted on a supported -3.16% against a 3% minimum, and 60 fresh pairs put
// it at -2.17%. A win well past the threshold needs no second look.
func BorderlineImprovements(config Config, eligible []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	if !config.StatisticalSupportRequired {
		return nil
	}
	best, supported := bestImprovement(comparisonResults(eligible))
	if !supported || !improvedAtLeastMinimum(config, best) || -ImprovementDelta(best) >= borderlineFactor*config.MinimumImprovementPercent {
		return nil
	}
	return []domain.MetricComparison{best}
}

// borderlineFactor bounds a borderline accept: a best supported improvement
// that cleared the minimum by less than this factor.
const borderlineFactor = 2.0

// SelectedImprovements returns the reading an accept would rest on when that
// reading is one workload's rather than the pooled metric, or nothing when the
// verdict would rest on the aggregate, would not accept, or when the manifest
// does not ask for statistical support. Such a reading is the best of several
// and was chosen because it read best, so its point estimate is biased upward
// (the winner's curse); the engine measures that workload again from scratch
// and lets the verdict rest on the fresh series alone (ADR 0041).
func SelectedImprovements(config Config, eligible []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	if !config.StatisticalSupportRequired || len(eligible) == 0 {
		return nil
	}
	best, supported := bestImprovement(comparisonResults(eligible))
	if !supported || best.Workload == "" || !improvedAtLeastMinimum(config, best) {
		return nil
	}
	return []domain.MetricComparison{best}
}

// improvedAtLeastMinimum reports whether a comparison's improving delta —
// the robust shift when one was measured, the mean otherwise — is at least as
// large in magnitude as the manifest's minimum_improvement_percent, and the two
// estimators agree it is an improvement. It reads the same delta and the same
// agreement rule acceptance does, so the confirmation triggers and the verdict
// cannot disagree about which win is borderline.
func improvedAtLeastMinimum(config Config, comparison domain.MetricComparison) bool {
	delta := ImprovementDelta(comparison)
	return estimatorsAgree(comparison) && finite(delta) && -delta >= config.MinimumImprovementPercent
}

// decideOnImprovement accepts on the best supported win in the eligible set.
// The best unsupported improvement still shapes the reason, so an operator can
// see how close an unsupported candidate came.
func decideOnImprovement(config Config, eligible []domain.MetricComparison, result Result) Result {
	best, supported := bestImprovement(eligible)
	improvement := -ImprovementDelta(best)
	if !supported {
		return inconclusive(result, fmt.Sprintf("no acceptance-eligible measurement has a statistically supported %q improvement (best %.2f%%%s)", config.PrimaryMetric, improvement, estimatorNote(best)))
	}
	if improvement < config.MinimumImprovementPercent {
		return inconclusive(result, fmt.Sprintf("best acceptance-eligible measurement improved by %.2f%%%s, below the %.2f%% threshold", improvement, estimatorNote(best), config.MinimumImprovementPercent))
	}
	if !estimatorsAgree(best) {
		return inconclusive(result, fmt.Sprintf("best acceptance-eligible measurement reads %.2f%%%s beside a mean delta of %+.2f%%, so the two estimators disagree on whether it improved", improvement, estimatorNote(best), best.DeltaPercent))
	}
	result.Decision = domain.DecisionAccepted
	result.Reasons = []string{fmt.Sprintf("%s improved by %.2f%%%s with required evidence and no guardrail regression", acceptanceSubject(best), improvement, estimatorNote(best))}
	return result
}

// estimatorNote names the robust estimator in a verdict sentence, so a reason
// that reads a smaller number than the recorded mean delta says why. It is
// empty when acceptance fell back to the mean.
func estimatorNote(comparison domain.MetricComparison) string {
	if hasRobustShift(comparison) {
		return " (Hodges–Lehmann shift)"
	}
	return ""
}

// bestImprovement returns the largest improvement in the eligible set together
// with whether that comparison carried statistical support. The supported
// comparison wins when one exists, because that is the one acceptance rests
// on. A reading whose mean delta contradicts its shift cannot carry an accept,
// so it is skipped while any other reading is left: ranking it first would hide
// an agreeing win behind it. When every reading disagrees, they all compete, so
// the verdict names the largest shift it refused.
func bestImprovement(eligible []domain.MetricComparison) (domain.MetricComparison, bool) {
	candidates := eligible
	agreeing := make([]domain.MetricComparison, 0, len(eligible))
	for _, comparison := range eligible {
		if estimatorsAgree(comparison) {
			agreeing = append(agreeing, comparison)
		}
	}
	if len(agreeing) > 0 {
		candidates = agreeing
	}
	best := candidates[0]
	supported := best.StatisticallyFit
	for _, comparison := range candidates[1:] {
		better := ImprovementDelta(comparison) < ImprovementDelta(best)
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

func checkGuardrails(config Config, byName map[string]domain.MetricComparison, evidence Evidence, result Result) (Result, bool) {
	for _, guardrail := range config.Guardrails {
		if early, done := checkOneGuardrail(config, guardrail, byName, evidence, result); done {
			return early, true
		}
	}
	return result, false
}

func checkOneGuardrail(config Config, guardrail Guardrail, byName map[string]domain.MetricComparison, evidence Evidence, result Result) (Result, bool) {
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
	// A guardrail past its limit rejects when the difference is significant,
	// whenever the manifest asks for statistical support, the rule ADR 0016
	// set for workload regressions. On the point estimate alone, 20 null
	// candidates on gron (a comment line each, identical code) lost 2 to
	// cpu_time_ns readings of +2.16% and +3.06% that were not significant: a
	// 12% false-rejection rate. A consistent regression is significant, even
	// with no spread at all, so a real one still rejects; an insignificant one
	// is named in the reasons and never makes the candidate inconclusive,
	// which is what once blocked a supported -14.38% win over a +0.16% memory
	// reading. The engine measures again first (UnconfirmedGuardrails).
	limit := guardrailLimit(config, guardrail)
	// Peak memory is the one guardrail whose reading a single series can get
	// wrong even with support: it is a per-run high-water mark, so one
	// contended run or one GC-pacing step moves it in megabytes. Such a
	// breach rejects only when a fresh, independent series reproduces it. A
	// caller that ran no fresh series keeps the old rule, so the leniency
	// cannot be reached by forgetting to measure.
	if replicatedGuardrail(guardrail.Name) && config.StatisticalSupportRequired {
		return checkReplicatedGuardrail(config, guardrail.Name, byName, evidence, limit, result)
	}
	if !breached(config, comparison, limit) {
		return result, false
	}
	return reject(result, breachReason(comparison, limit)), true
}

// checkReplicatedGuardrail decides a peak-memory breach from the fresh series'
// own readings when one was measured, and from the first series when none was.
//
// The breach has to be on a reading the first series produced — its own pooled
// reading, or the pooled one once the fresh series was folded in — but the
// decision is the fresh reading's. Deciding on the pooled reading alone would
// accept a regression both series reproduce: two series each reading +5%, at
// 30 MB and 40 MB, are a |t| above 100 on their own and about 1.7 pooled, so
// the pooled reading is not significant and, under this function's earlier
// order, the verdict never read the fresh series at all. A breach the fresh
// series does not reproduce does not reject, and the verdict continues to the
// next guardrail.
func checkReplicatedGuardrail(config Config, name string, byName map[string]domain.MetricComparison, evidence Evidence, limit float64, result Result) (Result, bool) {
	first, found := firstSeriesGuardrail(evidence, byName, name)
	breach := first
	onFirstSeries := found && breached(config, first, limit)
	if !onFirstSeries {
		pooled, ok := byName[name]
		if !ok || !breached(config, pooled, limit) {
			return result, false
		}
		breach = pooled
	}
	fresh, measured := freshGuardrail(evidence, name)
	return decideReplicatedBreach(config, replicatedBreach{
		name: name, limit: limit, reading: breach, onFirstSeries: onFirstSeries,
		fresh: fresh, measured: measured,
	}, result)
}

// replicatedBreach is one peak-memory breach resolved to the readings that
// decide it: the reading that breached, and the fresh series' own reading of
// the same measurement when one was measured. workload is the stress seed the
// readings belong to, and empty for the pooled guardrails.
type replicatedBreach struct {
	name          string
	workload      string
	limit         float64
	reading       domain.MetricComparison
	onFirstSeries bool
	fresh         domain.MetricComparison
	measured      bool
}

// decideReplicatedBreach applies ADR 0040 to one peak-memory breach: a reading
// over its limit with support rejects only when a fresh series of the same
// measurement breaches too. A caller that supplied no fresh reading keeps the
// single-series rule, so the leniency cannot be reached by forgetting to
// measure. Pooled guardrails and a stress seed's share this decision; only the
// reason differs.
func decideReplicatedBreach(config Config, breach replicatedBreach, result Result) (Result, bool) {
	if !breach.measured {
		return reject(result, breach.reason()+breach.note()), true
	}
	if !breached(config, breach.fresh, breach.limit) {
		return result, false
	}
	return reject(result, fmt.Sprintf("%s, and a fresh series reproduced it at %+.2f%%%s", breach.reason(), breach.fresh.DeltaPercent, breach.note())), true
}

// reason says which reading breached: the first series' own, the pooled one
// that became significant only once the fresh series was folded in, or one
// stress workload's.
func (b replicatedBreach) reason() string {
	if b.workload != "" {
		return stressBreachReason(b.reading, b.limit)
	}
	if b.onFirstSeries && b.measured {
		return fmt.Sprintf("guardrail %q read %+.2f%% on the first series, over the %.2f%% limit and significant", b.name, b.reading.DeltaPercent, b.limit)
	}
	return breachReason(b.reading, b.limit)
}

// note closes the reason for a reading on a workload the manifest keeps out of
// acceptance, and is empty for the pooled guardrails.
func (b replicatedBreach) note() string {
	if b.workload != "" {
		return stressBreachNote
	}
	return ""
}

func breachReason(comparison domain.MetricComparison, limit float64) string {
	return fmt.Sprintf("guardrail %q regressed by %.2f%%, over the %.2f%% limit", comparison.Metric, comparison.DeltaPercent, limit)
}

// breached reports whether one guardrail reading is past its limit with the
// support the manifest asks for.
func breached(config Config, comparison domain.MetricComparison, limit float64) bool {
	return comparison.DeltaPercent > limit && (comparison.Significant || !config.StatisticalSupportRequired)
}

// replicatedGuardrail reports whether a guardrail's breach has to reproduce
// in a fresh series before it rejects.
//
// Peak memory is a high-water mark rather than a steady rate: it steps with
// allocator and GC pacing, so the same two binaries can read +5.58% in one
// 60-pair verification and -0.68% in the next, and a candidate that only
// removed garbage lost a supported -3.4% wall-time win to a +2.78% reading.
// CPU time and binary size are left on the single-series rule: CPU time's
// noise is per-run spread that more pairs average out (ADR 0033), and binary
// size compares exact sizes.
func replicatedGuardrail(name string) bool { return name == "peak_memory_bytes" }

// freshGuardrail finds the pooled reading a fresh series produced for one
// guardrail, and whether one was measured at all.
func freshGuardrail(evidence Evidence, name string) (domain.MetricComparison, bool) {
	fresh := indexComparisons(comparisonResults(evidence.FreshGuardrails))
	comparison, found := fresh[name]
	return comparison, found
}

// unreplicatedBreach names a guardrail reading that was over its limit with
// support and not on the fresh one, so the verdict neither rejects on it nor
// hides it. pooled says the breach is the pooled reading rather than the first
// series' own: the reading only became significant once the fresh series was
// folded in.
type unreplicatedBreach struct {
	name       string
	limit      float64
	comparison domain.MetricComparison
	fresh      domain.MetricComparison
	pooled     bool
}

// unreplicatedReason says which reading breached, and what the fresh series
// read instead, so a reader of the verdict can see why nothing rejected.
func unreplicatedReason(g unreplicatedBreach) string {
	if g.pooled {
		return fmt.Sprintf("guardrail %q read %+.2f%% over the %.2f%% limit and significant only once the fresh series was pooled in, but that series read %+.2f%% and did not reproduce the breach", g.name, g.comparison.DeltaPercent, g.limit, g.fresh.DeltaPercent)
	}
	if g.comparison.Workload != "" {
		return fmt.Sprintf("guardrail %q regressed by %.2f%% on workload %q, over the %.2f%% limit and significant, but a fresh series read %+.2f%% and did not reproduce the breach%s", g.name, g.comparison.DeltaPercent, g.comparison.Workload, g.limit, g.fresh.DeltaPercent, stressBreachNote)
	}
	return fmt.Sprintf("guardrail %q read %+.2f%% on the first series, over the %.2f%% limit and significant, but a fresh series read %+.2f%% and did not reproduce the breach", g.name, g.comparison.DeltaPercent, g.limit, g.fresh.DeltaPercent)
}

// unreplicatedGuardrails returns the significant breaches a fresh series did
// not reproduce, for the verdict's reasons.
func unreplicatedGuardrails(config Config, byName map[string]domain.MetricComparison, evidence Evidence) []unreplicatedBreach {
	if !config.StatisticalSupportRequired {
		return nil
	}
	var out []unreplicatedBreach
	for _, guardrail := range config.Guardrails {
		if !replicatedGuardrail(guardrail.Name) {
			continue
		}
		if breach, ok := unreplicatedGuardrail(config, guardrail, byName, evidence); ok {
			out = append(out, breach)
		}
	}
	return out
}

// unreplicatedGuardrail judges one re-measured guardrail. The reading the
// fresh series did not reproduce is the first series' own when that series
// already breached, and the pooled one otherwise: a peak-memory reading over
// its limit without significance is re-measured as an unconfirmed guardrail,
// and folding the fresh series in can be what makes it significant.
func unreplicatedGuardrail(config Config, guardrail Guardrail, byName map[string]domain.MetricComparison, evidence Evidence) (unreplicatedBreach, bool) {
	limit := guardrailLimit(config, guardrail)
	fresh, measured := freshGuardrail(evidence, guardrail.Name)
	if !measured || breached(config, fresh, limit) {
		return unreplicatedBreach{}, false
	}
	if first, found := firstSeriesGuardrail(evidence, byName, guardrail.Name); found && breached(config, first, limit) {
		return unreplicatedBreach{name: guardrail.Name, limit: limit, comparison: first, fresh: fresh}, true
	}
	pooled, found := byName[guardrail.Name]
	if !found || !breached(config, pooled, limit) {
		return unreplicatedBreach{}, false
	}
	return unreplicatedBreach{name: guardrail.Name, limit: limit, comparison: pooled, fresh: fresh, pooled: true}, true
}

// firstSeriesGuardrail resolves the reading the first series produced for one
// guardrail. Evidence.FirstSeriesGuardrails is authoritative when it is there:
// a fresh series was measured, so the pooled Comparisons no longer hold the
// first series' value, and a reason quoting it must not read them. Without it
// no fresh series ran, and the pooled reading is the first series' own.
func firstSeriesGuardrail(evidence Evidence, byName map[string]domain.MetricComparison, name string) (domain.MetricComparison, bool) {
	if len(evidence.FirstSeriesGuardrails) > 0 {
		comparison, found := indexComparisons(comparisonResults(evidence.FirstSeriesGuardrails))[name]
		return comparison, found
	}
	comparison, found := byName[name]
	return comparison, found
}

// ReplicatedGuardrails returns the pooled readings of the guardrails whose
// breach has to reproduce in a fresh series (replicatedGuardrail), breach or
// not. The engine records them on the evidence before it measures that series:
// once the fresh readings are folded in, Comparisons hold one value for both
// series, and a reason that says what the first series read has to quote what
// that series produced.
func ReplicatedGuardrails(config Config, comparisons []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	byName := indexComparisons(comparisonResults(comparisons))
	var readings []domain.MetricComparison
	for _, guardrail := range config.Guardrails {
		if !replicatedGuardrail(guardrail.Name) {
			continue
		}
		if comparison, found := byName[guardrail.Name]; found {
			readings = append(readings, comparison)
		}
	}
	return readings
}

// GuardrailsToReplicate returns the pooled guardrail readings that are over
// their limit with support, on a guardrail whose breach has to reproduce in a
// fresh series (replicatedGuardrail) before Evaluate rejects on it. The engine
// measures that series when there are any, as it does for an unconfirmed
// reading. Every confirmation series records its own guardrail readings in
// Evidence.FreshGuardrails, whatever asked for it.
func GuardrailsToReplicate(config Config, comparisons []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	if !config.StatisticalSupportRequired {
		return nil
	}
	byName := indexComparisons(comparisonResults(comparisons))
	var readings []domain.MetricComparison
	for _, guardrail := range config.Guardrails {
		if !replicatedGuardrail(guardrail.Name) {
			continue
		}
		comparison, found := byName[guardrail.Name]
		limit := guardrailLimit(config, guardrail)
		if found && comparison.DeltaPercent > limit && comparison.Significant {
			readings = append(readings, comparison)
		}
	}
	return readings
}

// StressGuardrailsToReplicate returns the stress-workload readings that are
// over their limit with support, on a guardrail whose breach has to reproduce
// in a fresh series before Evaluate rejects on it (replicatedGuardrail). The
// engine measures that series on the same seed, as GuardrailsToReplicate does
// for the pooled readings. A reading without support returns nothing: the
// policy names it in the reasons instead, so no series is spent on it.
func StressGuardrailsToReplicate(config Config, comparisons []domain.MetricComparison) []domain.MetricComparison {
	config = withDefaults(config)
	if !config.StatisticalSupportRequired {
		return nil
	}
	var readings []domain.MetricComparison
	for _, comparison := range comparisonResults(comparisons) {
		guardrail, ok := guardrailNamed(config, comparison.Metric)
		if !ok || !replicatedGuardrail(guardrail.Name) {
			continue
		}
		if breached(config, comparison, guardrailLimit(config, guardrail)) {
			readings = append(readings, comparison)
		}
	}
	return readings
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

// RegressionLimit returns the regression ceiling one metric is judged by: the
// guardrail's own limit when the manifest gives it one — which is where
// --tradeoff and --allow land — and the config's shared ceiling otherwise.
// Callers outside the verdict use it to reason about the same bound Evaluate
// applies, rather than a copy of the default.
func RegressionLimit(config Config, metric string) float64 {
	config = withDefaults(config)
	for _, guardrail := range config.Guardrails {
		if guardrail.Name == metric {
			return guardrailLimit(config, guardrail)
		}
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
