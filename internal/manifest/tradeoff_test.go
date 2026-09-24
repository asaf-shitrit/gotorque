package manifest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// gronPerformance is the performance block every shipped manifest carries.
func gronPerformance() PerformancePolicy {
	return PerformancePolicy{
		PrimaryMetric: "wall_time_ns", MinimumImprovementPercent: 3, MaximumGuardrailRegressionPercent: 2,
		Guardrails: []Guardrail{
			{Name: "peak_memory_bytes", MaximumRegressionPercent: 2, Required: true},
			{Name: "cpu_time_ns", MaximumRegressionPercent: 2, Required: true},
			{Name: "binary_size_bytes", MaximumRegressionPercent: 5},
		},
	}
}

func guardrail(p PerformancePolicy, name string) (Guardrail, bool) {
	for _, g := range p.Guardrails {
		if g.Name == name {
			return g, true
		}
	}
	return Guardrail{}, false
}

func TestSpeedLetsMemoryAndCPURegress(t *testing.T) {
	speed, err := ResolveTradeoff("speed", nil)
	require.NoError(t, err)
	p, err := speed.Apply(gronPerformance())
	require.NoError(t, err)
	require.Equal(t, "wall_time_ns", p.PrimaryMetric)
	mem, _ := guardrail(p, "peak_memory_bytes")
	cpu, _ := guardrail(p, "cpu_time_ns")
	size, _ := guardrail(p, "binary_size_bytes")
	require.InDelta(t, 10, mem.MaximumRegressionPercent, 0)
	require.InDelta(t, 5, cpu.MaximumRegressionPercent, 0)
	require.Equal(t, Guardrail{Name: "binary_size_bytes", MaximumRegressionPercent: 5}, size, "metrics the preset does not name keep the manifest's limit")
	require.Len(t, gronPerformance().Guardrails, 3, "the manifest's block is not modified")
}

func TestLeanImprovesMemoryAndGuardsWallTime(t *testing.T) {
	lean, err := ResolveTradeoff("lean", nil)
	require.NoError(t, err)
	p, err := lean.Apply(gronPerformance())
	require.NoError(t, err)
	require.Equal(t, "peak_memory_bytes", p.PrimaryMetric)
	_, memoryGuarded := guardrail(p, "peak_memory_bytes")
	require.False(t, memoryGuarded, "the objective is not also a guardrail")
	wall, ok := guardrail(p, "wall_time_ns")
	require.True(t, ok)
	require.Equal(t, Guardrail{Name: "wall_time_ns", MaximumRegressionPercent: 3, Required: true}, wall)
}

func TestAnObjectiveSwitchGuardsTheOldObjective(t *testing.T) {
	p, err := Tradeoff{Objective: "cpu_time_ns"}.Apply(gronPerformance())
	require.NoError(t, err)
	wall, ok := guardrail(p, "wall_time_ns")
	require.True(t, ok)
	require.Equal(t, Guardrail{Name: "wall_time_ns", MaximumRegressionPercent: 2, Required: true}, wall, "at the manifest's guardrail limit")
}

func TestAllowancesOverrideAPreset(t *testing.T) {
	speed, err := ResolveTradeoff("speed", []string{"memory=5%", "binary_size_bytes=1"})
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"peak_memory_bytes": 5, "cpu_time_ns": 5, "binary_size_bytes": 1}, speed.Allow)
	p, err := speed.Apply(gronPerformance())
	require.NoError(t, err)
	size, _ := guardrail(p, "binary_size_bytes")
	require.Equal(t, Guardrail{Name: "binary_size_bytes", MaximumRegressionPercent: 1, Required: true}, size, "an allowance makes its guardrail required")

	custom, err := ResolveTradeoff("", []string{"cpu=4"})
	require.NoError(t, err)
	require.Equal(t, Tradeoff{Allow: map[string]float64{"cpu_time_ns": 4}}, custom)
	none, err := ResolveTradeoff("", nil)
	require.NoError(t, err)
	require.True(t, none.IsZero())
}

func TestTradeoffsRejectWhatCannotBeJudged(t *testing.T) {
	for _, c := range []struct {
		preset string
		allow  []string
		want   string
	}{
		{"fastest", nil, "unknown trade-off"},
		{"lean", []string{"memory=5%"}, "cannot allow peak_memory_bytes to regress while improving it"},
		{"speed", []string{"wall=1%"}, "cannot allow wall_time_ns to regress while improving it"},
		{"", []string{"latency=5%"}, "unknown metric"},
		{"balanced", []string{"memory=-1"}, "non-negative percent"},
		{"balanced", []string{"memory"}, "want metric=percent"},
	} {
		_, err := ResolveTradeoff(c.preset, c.allow)
		require.ErrorContains(t, err, c.want, "%q %v", c.preset, c.allow)
	}
}

func TestDescribeStatesTheRules(t *testing.T) {
	speed, err := ResolveTradeoff("speed", nil)
	require.NoError(t, err)
	p, err := speed.Apply(gronPerformance())
	require.NoError(t, err)
	require.Equal(t, "improve wall_time_ns by at least 3%; may regress at most peak_memory_bytes +10%, cpu_time_ns +5%, binary_size_bytes +5%", Describe(p))
}
