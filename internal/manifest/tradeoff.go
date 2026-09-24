package manifest

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Tradeoff is what one campaign may give up for its improvement: the metric it
// improves, and how far each other metric may regress. It overrides the
// manifest's performance block for one run, so a target's checked-in contract
// stays the default while an operator who would trade memory for speed, or the
// reverse, says so on the command line.
//
// A live go-jsonnet candidate cut wall time 3.47% and CPU time 2.42%, both
// supported, and was rejected for 2.43% more peak memory, over the manifest's
// 2% limit. The rejection was correct under that contract; whether an
// interpreter should trade memory for speed is the operator's choice, not the
// manifest author's.
type Tradeoff struct {
	// Name is the preset the trade-off started from, or empty for none.
	Name string `json:"name,omitempty"`
	// Objective is the metric to improve; empty keeps the manifest's.
	Objective string `json:"objective,omitempty"`
	// Allow is the largest regression, in percent, each other metric may
	// show. A metric named here becomes a required guardrail.
	Allow map[string]float64 `json:"allow,omitempty"`
}

// Metrics are the metrics the engine measures for every candidate.
var Metrics = []string{"wall_time_ns", "cpu_time_ns", "peak_memory_bytes", "binary_size_bytes"}

// metricAliases are the short names the command line accepts.
var metricAliases = map[string]string{
	"wall": "wall_time_ns", "speed": "wall_time_ns", "time": "wall_time_ns",
	"cpu": "cpu_time_ns", "memory": "peak_memory_bytes", "mem": "peak_memory_bytes",
	"size": "binary_size_bytes", "binary": "binary_size_bytes",
}

// Presets are the named trade-offs. balanced is the manifest as written.
var Presets = map[string]Tradeoff{
	"balanced": {Name: "balanced"},
	"speed": {Name: "speed", Objective: "wall_time_ns", Allow: map[string]float64{
		"peak_memory_bytes": 10, "cpu_time_ns": 5,
	}},
	"lean": {Name: "lean", Objective: "peak_memory_bytes", Allow: map[string]float64{
		"wall_time_ns": 3, "cpu_time_ns": 3,
	}},
}

// MetricName resolves a metric or one of its short names.
func MetricName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if alias, ok := metricAliases[name]; ok {
		return alias, nil
	}
	if slices.Contains(Metrics, name) {
		return name, nil
	}
	return "", fmt.Errorf("unknown metric %q: use one of %s, or wall, cpu, memory, size", name, strings.Join(Metrics, ", "))
}

// ParseAllowance reads one "metric=N%" allowance.
func ParseAllowance(spec string) (string, float64, error) {
	name, value, ok := strings.Cut(spec, "=")
	if !ok {
		return "", 0, fmt.Errorf("allowance %q: want metric=percent, like memory=5%%", spec)
	}
	metric, err := MetricName(name)
	if err != nil {
		return "", 0, err
	}
	percent, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "%"), 64)
	if err != nil || percent < 0 {
		return "", 0, fmt.Errorf("allowance %q: want a non-negative percent", spec)
	}
	return metric, percent, nil
}

// ResolveTradeoff starts from a preset, or from nothing when preset is empty,
// and lays the allowances over it. It returns the zero Tradeoff when neither
// is given, which leaves the manifest untouched.
func ResolveTradeoff(preset string, allowances []string) (Tradeoff, error) {
	var t Tradeoff
	if preset != "" {
		p, ok := Presets[preset]
		if !ok {
			return Tradeoff{}, fmt.Errorf("unknown trade-off %q: use %s", preset, strings.Join(slices.Sorted(maps.Keys(Presets)), ", "))
		}
		t = Tradeoff{Name: p.Name, Objective: p.Objective, Allow: maps.Clone(p.Allow)}
	}
	for _, spec := range allowances {
		metric, percent, err := ParseAllowance(spec)
		if err != nil {
			return Tradeoff{}, err
		}
		if t.Allow == nil {
			t.Allow = map[string]float64{}
		}
		t.Allow[metric] = percent
	}
	if t.Objective != "" && hasKey(t.Allow, t.Objective) {
		return Tradeoff{}, fmt.Errorf("cannot allow %s to regress while improving it", t.Objective)
	}
	return t, nil
}

func hasKey(m map[string]float64, key string) bool {
	_, ok := m[key]
	return ok
}

// IsZero reports a trade-off that changes nothing.
func (t Tradeoff) IsZero() bool { return t.Objective == "" && len(t.Allow) == 0 }

// Apply returns the performance block this trade-off judges candidates by.
// Switching the objective makes the old primary metric a required guardrail,
// at the manifest's guardrail limit unless an allowance says otherwise, and
// removes the new objective from the guardrails. Each allowance sets its
// metric's guardrail, adding a required one when the manifest had none.
func (t Tradeoff) Apply(p PerformancePolicy) (PerformancePolicy, error) {
	out := p
	out.Guardrails = slices.Clone(p.Guardrails)
	if t.Objective != "" && t.Objective != p.PrimaryMetric {
		old := p.PrimaryMetric
		out.PrimaryMetric = t.Objective
		out.Guardrails = slices.DeleteFunc(out.Guardrails, func(g Guardrail) bool { return g.Name == t.Objective })
		if !slices.ContainsFunc(out.Guardrails, func(g Guardrail) bool { return g.Name == old }) {
			out.Guardrails = append(out.Guardrails, Guardrail{Name: old, MaximumRegressionPercent: p.MaximumGuardrailRegressionPercent, Required: true})
		}
	}
	for _, metric := range slices.Sorted(maps.Keys(t.Allow)) {
		if metric == out.PrimaryMetric {
			return PerformancePolicy{}, fmt.Errorf("cannot allow %s to regress while improving it", metric)
		}
		out.Guardrails = setGuardrail(out.Guardrails, metric, t.Allow[metric])
	}
	return out, nil
}

func setGuardrail(guardrails []Guardrail, metric string, percent float64) []Guardrail {
	if i := slices.IndexFunc(guardrails, func(g Guardrail) bool { return g.Name == metric }); i >= 0 {
		guardrails[i].MaximumRegressionPercent = percent
		guardrails[i].Required = true
		return guardrails
	}
	return append(guardrails, Guardrail{Name: metric, MaximumRegressionPercent: percent, Required: true})
}

// Describe states, in one line, what a candidate is judged by under p.
func Describe(p PerformancePolicy) string {
	parts := make([]string, 0, len(p.Guardrails))
	for _, g := range p.Guardrails {
		limit := g.MaximumRegressionPercent
		if limit == 0 {
			limit = p.MaximumGuardrailRegressionPercent
		}
		parts = append(parts, fmt.Sprintf("%s +%g%%", g.Name, limit))
	}
	line := fmt.Sprintf("improve %s by at least %g%%", p.PrimaryMetric, p.MinimumImprovementPercent)
	if len(parts) == 0 {
		return line
	}
	return line + "; may regress at most " + strings.Join(parts, ", ")
}

// ErrTradeoffOnResume refuses to change a campaign's rules midway.
var ErrTradeoffOnResume = errors.New("--tradeoff and --allow cannot be changed on resume: a campaign keeps the trade-off it started with")
