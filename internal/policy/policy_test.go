package policy

import (
	"math"
	"strings"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/domain"
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
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 103, StatisticallyFit: true, Significant: true},
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

// TestEvaluateGuardrailOverLimitRejectsOnlyWhenSignificant: past its limit,
// a guardrail rejects when the difference is significant, or when the
// manifest does not ask for statistical support. An insignificant reading is
// named in the reasons and neither rejects nor blocks the verdict: 20 null
// candidates on gron lost 2 to cpu_time_ns noise of +2.16% and +3.06%.
func TestEvaluateGuardrailOverLimitRejectsOnlyWhenSignificant(t *testing.T) {
	evidence := func(significant bool) Evidence {
		return Evidence{
			BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
			Comparisons: []domain.MetricComparison{
				{Metric: "wall_time_ns", Baseline: 100, Candidate: 85, StatisticallyFit: true},
				{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
				{Metric: "cpu_time_ns", Baseline: 100, Candidate: 103.06, StatisticallyFit: false, Significant: significant},
				{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			},
		}
	}
	result := Evaluate(DefaultConfig(), evidence(false))
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("insignificant reading: decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if !strings.Contains(strings.Join(result.Reasons, "; "), `guardrail "cpu_time_ns" read +3.06%, over the 2.00% limit, but the difference is not statistically significant`) {
		t.Fatalf("the insignificant reading is not named: %v", result.Reasons)
	}
	if got := UnconfirmedGuardrails(DefaultConfig(), evidence(false).Comparisons); len(got) != 1 || got[0].Metric != "cpu_time_ns" {
		t.Fatalf("UnconfirmedGuardrails = %+v, want the cpu_time_ns reading", got)
	}

	if result := Evaluate(DefaultConfig(), evidence(true)); result.Decision != domain.DecisionRejected {
		t.Fatalf("significant reading: decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if got := UnconfirmedGuardrails(DefaultConfig(), evidence(true).Comparisons); len(got) != 0 {
		t.Fatalf("a significant reading is not unconfirmed: %+v", got)
	}

	pointEstimate := DefaultConfig()
	pointEstimate.StatisticalSupportRequired = false
	if result := Evaluate(pointEstimate, evidence(false)); result.Decision != domain.DecisionRejected {
		t.Fatalf("without required support the limit decides alone: decision = %s", result.Decision)
	}
	if got := UnconfirmedGuardrails(pointEstimate, evidence(false).Comparisons); len(got) != 0 {
		t.Fatalf("nothing is unconfirmed without required support: %+v", got)
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
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 41, StatisticallyFit: true, Significant: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{
			{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57, StatisticallyFit: true},
			{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 41, StatisticallyFit: true, Significant: true},
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

// liveCampaignEvidence is gron campaign #4's first candidate: the output loop
// buffered, flatten-users -20.41% with support, small-doc +4.01% at p above
// 0.05. The candidate was rejected on the small-doc point estimate.
func liveCampaignEvidence(smallDoc domain.MetricComparison) Evidence {
	flatten := domain.MetricComparison{Metric: "wall_time_ns", Workload: "flatten-users", Baseline: 27.33, Candidate: 21.75, StatisticallyFit: true, Significant: true}
	pooled := domain.MetricComparison{Metric: "wall_time_ns", Baseline: 20.81, Candidate: 18.31, StatisticallyFit: true, Significant: true}
	return Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			pooled, flatten, smallDoc,
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100.26, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 88.17, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		Primary: []domain.MetricComparison{pooled, flatten, smallDoc},
	}
}

func TestEvaluateDoesNotRejectOnAnInsignificantRegression(t *testing.T) {
	smallDoc := domain.MetricComparison{Metric: "wall_time_ns", Workload: "small-doc", Baseline: 14.29, Candidate: 14.8631, StatisticallyFit: false}
	result := Evaluate(DefaultConfig(), liveCampaignEvidence(smallDoc))
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if len(result.Reasons) != 2 || !strings.Contains(result.Reasons[0], `workload "flatten-users" improved by 20.42%`) {
		t.Fatalf("the verdict should rest on flatten-users: %v", result.Reasons)
	}
	if !strings.Contains(result.Reasons[1], `workload "small-doc" read +4.01%, over the 2.00% limit, but the difference is not statistically significant`) {
		t.Fatalf("the unconfirmed regression should be named: %v", result.Reasons)
	}
}

func TestEvaluateRejectsASignificantRegression(t *testing.T) {
	smallDoc := domain.MetricComparison{Metric: "wall_time_ns", Workload: "small-doc", Baseline: 13.34, Candidate: 14.123, Significant: true}
	result := Evaluate(DefaultConfig(), liveCampaignEvidence(smallDoc))
	if result.Decision != domain.DecisionRejected || !strings.Contains(result.Reasons[0], `workload "small-doc" regressed by +5.87%`) {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
}

// A manifest that does not ask for statistical support keeps the point
// estimate as the whole rule, for regressions as for wins.
func TestEvaluateRejectsOnThePointEstimateWithoutRequiredSupport(t *testing.T) {
	config := DefaultConfig()
	config.StatisticalSupportRequired = false
	smallDoc := domain.MetricComparison{Metric: "wall_time_ns", Workload: "small-doc", Baseline: 14.29, Candidate: 14.8631}
	if result := Evaluate(config, liveCampaignEvidence(smallDoc)); result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if got := UnconfirmedRegressions(config, liveCampaignEvidence(smallDoc).Primary); len(got) != 0 {
		t.Fatalf("nothing to confirm when the point estimate decides: %v", got)
	}
}

func TestUnconfirmedRegressionsAreOverTheLimitAndInsignificant(t *testing.T) {
	primary := []domain.MetricComparison{
		{Metric: "wall_time_ns", Baseline: 100, Candidate: 103},
		{Metric: "wall_time_ns", Workload: "big", Baseline: 60, Candidate: 57, Significant: true},
		{Metric: "wall_time_ns", Workload: "small", Baseline: 40, Candidate: 42, Significant: true},
		{Metric: "wall_time_ns", Workload: "tiny", Baseline: 10, Candidate: 10.1},
	}
	got := UnconfirmedRegressions(Config{StatisticalSupportRequired: true}, primary)
	if len(got) != 1 || got[0].Workload != "" || math.Abs(got[0].DeltaPercent-3) > 1e-9 {
		t.Fatalf("UnconfirmedRegressions = %+v, want only the pooled +3%% reading", got)
	}
}

// TestBorderlineImprovementsSelectsOnlyANearThresholdAccept: the reading an
// accept would rest on is returned when it cleared the 3% minimum by less
// than twice it; a clear win, an unsupported reading, one below the minimum,
// or a manifest without required support returns nothing.
func TestBorderlineImprovementsSelectsOnlyANearThresholdAccept(t *testing.T) {
	reading := func(candidate float64, fit bool) []domain.MetricComparison {
		return []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "w", Baseline: 100, Candidate: candidate, StatisticallyFit: fit}}
	}
	if got := BorderlineImprovements(DefaultConfig(), reading(96.84, true)); len(got) != 1 || got[0].Workload != "w" {
		t.Fatalf("a supported -3.16%% is borderline: %+v", got)
	}
	for name, eligible := range map[string][]domain.MetricComparison{
		"clear win":     reading(90, true),
		"unsupported":   reading(96.84, false),
		"below minimum": reading(98, true),
	} {
		if got := BorderlineImprovements(DefaultConfig(), eligible); len(got) != 0 {
			t.Fatalf("%s: got %+v, want nothing", name, got)
		}
	}
	noSupport := DefaultConfig()
	noSupport.StatisticalSupportRequired = false
	if got := BorderlineImprovements(noSupport, reading(96.84, true)); len(got) != 0 {
		t.Fatalf("without required support nothing is re-measured: %+v", got)
	}
}

// TestRegressionLimitIsTheMetricsOwnCeiling: the equivalence check that grants
// support to a non-improving reading is bounded by the same per-metric limit
// the verdict applies, so a manifest's narrower limit, a trade-off or an
// --allow reaches it, and a metric with no guardrail of its own falls back to
// the shared ceiling.
func TestRegressionLimitIsTheMetricsOwnCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaximumGuardrailRegressionPercent = 2.5
	config.Guardrails = []Guardrail{
		{Name: "peak_memory_bytes", MaximumRegressionPercent: 5, Required: true},
		{Name: "cpu_time_ns"}, // no own limit: the shared ceiling applies
	}
	if got := RegressionLimit(config, "peak_memory_bytes"); got != 5 {
		t.Fatalf("RegressionLimit(peak_memory_bytes) = %v, want the guardrail's own 5", got)
	}
	if got := RegressionLimit(config, "cpu_time_ns"); got != 2.5 {
		t.Fatalf("RegressionLimit(cpu_time_ns) = %v, want the shared 2.5", got)
	}
	if got := RegressionLimit(config, "wall_time_ns"); got != 2.5 {
		t.Fatalf("RegressionLimit(wall_time_ns) = %v, want the shared 2.5 for a metric with no guardrail", got)
	}
	if got := RegressionLimit(Config{}, "peak_memory_bytes"); got != DefaultMaximumGuardrailRegressionPercent {
		t.Fatalf("RegressionLimit on an empty config = %v, want the default", got)
	}
}

// TestSelectedImprovementsReturnsOnlyAPerWorkloadAccept: the reading an accept
// would rest on is returned when it is one workload's rather than the pooled
// metric, and nothing when the aggregate carries the accept, the reading is
// unsupported or below the minimum, or the manifest does not require support.
func TestSelectedImprovementsReturnsOnlyAPerWorkloadAccept(t *testing.T) {
	workload := func(candidate float64, fit bool) domain.MetricComparison {
		return domain.MetricComparison{Metric: "wall_time_ns", Workload: "w", Baseline: 100, Candidate: candidate, StatisticallyFit: fit}
	}
	pooled := func(candidate float64) domain.MetricComparison {
		return domain.MetricComparison{Metric: "wall_time_ns", Baseline: 100, Candidate: candidate, StatisticallyFit: true}
	}
	if got := SelectedImprovements(DefaultConfig(), []domain.MetricComparison{pooled(98), workload(93.49, true)}); len(got) != 1 || got[0].Workload != "w" {
		t.Fatalf("a supported workload -6.51%% carries the accept: %+v", got)
	}
	if got := SelectedImprovements(DefaultConfig(), []domain.MetricComparison{pooled(90), workload(98, true)}); len(got) != 0 {
		t.Fatalf("an accept the pooled metric carries chooses no workload reading: %+v", got)
	}
	for name, eligible := range map[string][]domain.MetricComparison{
		"unsupported":   {pooled(98), workload(91, false)},
		"below minimum": {pooled(98), workload(98, true)},
		"empty":         {},
	} {
		if got := SelectedImprovements(DefaultConfig(), eligible); len(got) != 0 {
			t.Fatalf("%s: got %+v, want nothing", name, got)
		}
	}
	noSupport := DefaultConfig()
	noSupport.StatisticalSupportRequired = false
	if got := SelectedImprovements(noSupport, []domain.MetricComparison{pooled(98), workload(93.49, true)}); len(got) != 0 {
		t.Fatalf("without required support the verdict rests on the point estimate: %+v", got)
	}
}

// peakMemoryEvidence is a candidate that doubles nothing: a supported wall-time
// win, a flat CPU time, an unchanged binary, and a peak-memory reading of the
// caller's choosing.
func peakMemoryEvidence(memory, freshMemory domain.MetricComparison, fresh bool) Evidence {
	evidence := Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 80, StatisticallyFit: true},
			memory,
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
	}
	if fresh {
		evidence.FreshGuardrails = []domain.MetricComparison{freshMemory}
	}
	return evidence
}

// TestASignificantPeakMemoryBreachNeedsAFreshSeries: peak memory is a per-run
// high-water mark, so a supported breach on one series is not enough to reject.
// The same two binaries read +5.58% on one 60-pair verification and -0.68% on
// the next, and a candidate that only removed garbage lost a supported -3.4%
// wall-time win to a +2.78% reading it did not cause.
func TestASignificantPeakMemoryBreachNeedsAFreshSeries(t *testing.T) {
	breach := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546091, StatisticallyFit: true, Significant: true}
	quiet := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 36507648, StatisticallyFit: false}
	repeated := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 39000000, StatisticallyFit: true, Significant: true}

	unmeasured := Evaluate(DefaultConfig(), peakMemoryEvidence(breach, quiet, false))
	if unmeasured.Decision != domain.DecisionRejected {
		t.Fatalf("with no fresh series the single-series rule holds: decision = %s, reasons = %v", unmeasured.Decision, unmeasured.Reasons)
	}

	didNotReproduce := Evaluate(DefaultConfig(), peakMemoryEvidence(breach, quiet, true))
	if didNotReproduce.Decision != domain.DecisionAccepted {
		t.Fatalf("a breach a fresh series did not reproduce must not reject the win: decision = %s, reasons = %v", didNotReproduce.Decision, didNotReproduce.Reasons)
	}
	reasons := strings.Join(didNotReproduce.Reasons, "; ")
	if !strings.Contains(reasons, `guardrail "peak_memory_bytes" read +5.58% on the first series, over the 2.00% limit and significant, but a fresh series read +0.00% and did not reproduce the breach`) {
		t.Fatalf("the unreplicated breach is not named: %v", didNotReproduce.Reasons)
	}

	reproduced := Evaluate(DefaultConfig(), peakMemoryEvidence(breach, repeated, true))
	if reproduced.Decision != domain.DecisionRejected {
		t.Fatalf("a breach a fresh series reproduces still rejects: decision = %s, reasons = %v", reproduced.Decision, reproduced.Reasons)
	}
	if reasons := strings.Join(reproduced.Reasons, "; "); !strings.Contains(reasons, "and a fresh series reproduced it at +6.83%") {
		t.Fatalf("the rejection does not say the breach reproduced: %v", reproduced.Reasons)
	}
}

// TestAnUnreplicatedBreachQuotesTheFirstSeries: once a fresh series has run,
// the pooled comparisons hold one value for both series. The reason says "on
// the first series", so it quotes Evidence.FirstSeriesGuardrails: here the
// pooled +2.71% is the number a reader was shown under that label, and the
// first series read +5.58%.
func TestAnUnreplicatedBreachQuotesTheFirstSeries(t *testing.T) {
	firstSeries := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 38546091, StatisticallyFit: true, Significant: true}
	pooled := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 37497005, StatisticallyFit: true, Significant: true}
	quiet := domain.MetricComparison{Metric: "peak_memory_bytes", Baseline: 36507648, Candidate: 36507648, StatisticallyFit: true}
	evidence := peakMemoryEvidence(pooled, quiet, true)
	evidence.FirstSeriesGuardrails = []domain.MetricComparison{firstSeries}

	result := Evaluate(DefaultConfig(), evidence)
	if result.Decision != domain.DecisionAccepted {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	reasons := strings.Join(result.Reasons, "; ")
	if !strings.Contains(reasons, `read +5.58% on the first series`) {
		t.Fatalf("the reason does not quote what the first series read: %v", result.Reasons)
	}
	if strings.Contains(reasons, "+2.71%") {
		t.Fatalf("the reason quotes the pooled reading as the first series' own: %v", result.Reasons)
	}
}

// TestCPUAndSizeBreachesKeepTheSingleSeriesRule: only peak memory steps with
// allocator and GC pacing. A supported CPU-time breach rejects on one series,
// however the fresh series read.
func TestCPUAndSizeBreachesKeepTheSingleSeriesRule(t *testing.T) {
	evidence := Evidence{
		BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true,
		Comparisons: []domain.MetricComparison{
			{Metric: "wall_time_ns", Baseline: 100, Candidate: 80, StatisticallyFit: true},
			{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
			{Metric: "cpu_time_ns", Baseline: 100, Candidate: 106, StatisticallyFit: true, Significant: true},
			{Metric: "binary_size_bytes", Baseline: 100, Candidate: 100, StatisticallyFit: true},
		},
		FreshGuardrails: []domain.MetricComparison{{Metric: "cpu_time_ns", Baseline: 100, Candidate: 100, StatisticallyFit: true}},
	}
	result := Evaluate(DefaultConfig(), evidence)
	if result.Decision != domain.DecisionRejected {
		t.Fatalf("decision = %s, reasons = %v", result.Decision, result.Reasons)
	}
	if got := GuardrailsToReplicate(DefaultConfig(), evidence.Comparisons); len(got) != 0 {
		t.Fatalf("no guardrail but peak memory is replicated: %+v", got)
	}
}

// TestGuardrailsToReplicateNamesSupportedPeakMemoryBreaches: the engine runs a
// fresh series for exactly the readings the verdict would otherwise reject on
// from one series: a supported peak-memory breach.
func TestGuardrailsToReplicateNamesSupportedPeakMemoryBreaches(t *testing.T) {
	comparisons := []domain.MetricComparison{
		{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 105, StatisticallyFit: true, Significant: true},
	}
	got := GuardrailsToReplicate(DefaultConfig(), comparisons)
	if len(got) != 1 || got[0].Metric != "peak_memory_bytes" {
		t.Fatalf("GuardrailsToReplicate = %+v, want the peak_memory_bytes reading", got)
	}

	unsupported := []domain.MetricComparison{
		{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 105},
	}
	if got := GuardrailsToReplicate(DefaultConfig(), unsupported); len(got) != 0 {
		t.Fatalf("an unsupported reading is already handled by UnconfirmedGuardrails: %+v", got)
	}

	withinLimit := []domain.MetricComparison{
		{Metric: "peak_memory_bytes", Baseline: 100, Candidate: 101, StatisticallyFit: true, Significant: true},
	}
	if got := GuardrailsToReplicate(DefaultConfig(), withinLimit); len(got) != 0 {
		t.Fatalf("a reading within the limit is not a breach: %+v", got)
	}

	noSupport := DefaultConfig()
	noSupport.StatisticalSupportRequired = false
	if got := GuardrailsToReplicate(noSupport, comparisons); len(got) != 0 {
		t.Fatalf("without required support the point estimate decides alone: %+v", got)
	}
}
