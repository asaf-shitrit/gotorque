package campaign

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/policy"
)

// jsonnetCandidate is the go-jsonnet candidate a live campaign rejected: 3.47%
// faster and 2.42% less CPU, both supported, for 2.43% more peak memory.
func jsonnetCandidate() []domain.MetricComparison {
	reading := func(workload, metric string, delta float64) domain.MetricComparison {
		return domain.MetricComparison{Workload: workload, Metric: metric, Baseline: 100, Candidate: 100 + delta, DeltaPercent: delta, StatisticallyFit: true, Significant: true}
	}
	return []domain.MetricComparison{
		reading("", "wall_time_ns", -3.47),
		reading("fleet-config", "wall_time_ns", -3.40),
		reading("order-report", "wall_time_ns", -3.60),
		reading("", "cpu_time_ns", -2.42),
		reading("", "peak_memory_bytes", 2.43),
		reading("", "binary_size_bytes", 0),
	}
}

func judge(t *testing.T, tradeoff manifest.Tradeoff) policy.Result {
	t.Helper()
	m, err := manifest.LoadFile(filepath.Join("..", "..", "targets", "go-jsonnet", "manifest.json"))
	require.NoError(t, err)
	if !tradeoff.IsZero() {
		m.Performance, err = tradeoff.Apply(m.Performance)
		require.NoError(t, err)
	}
	config := policyConfigFromManifest(m)
	comparisons := jsonnetCandidate()
	return policy.Evaluate(config, policy.Evidence{BehaviorMatches: true, SafetyChecksPassed: true, RepresentativeEvidence: true, Comparisons: comparisons, Primary: eligibleReadings(config, comparisons)})
}

// TestTheSpeedTradeoffAcceptsWhatTheManifestRejects: under the manifest's own
// limits the memory guardrail rejects the candidate; under --tradeoff speed
// the same readings are accepted, and an explicit memory allowance below the
// regression rejects them again.
func TestTheSpeedTradeoffAcceptsWhatTheManifestRejects(t *testing.T) {
	balanced := judge(t, manifest.Tradeoff{})
	require.Equal(t, domain.DecisionRejected, balanced.Decision)
	require.Contains(t, strings.Join(balanced.Reasons, "; "), `guardrail "peak_memory_bytes" regressed by 2.43%, over the 2.00% limit`)

	speed, err := manifest.ResolveTradeoff("speed", nil)
	require.NoError(t, err)
	require.Equal(t, domain.DecisionAccepted, judge(t, speed).Decision)

	tight, err := manifest.ResolveTradeoff("speed", []string{"memory=2%"})
	require.NoError(t, err)
	require.Equal(t, domain.DecisionRejected, judge(t, tight).Decision)
}

// TestACampaignKeepsAndReportsItsTradeoff: the trade-off is applied once, at
// creation, to the campaign's copy of the manifest, which every verdict and a
// resume read; the report states it and reproduces it.
func TestACampaignKeepsAndReportsItsTradeoff(t *testing.T) {
	speed, err := manifest.ResolveTradeoff("speed", []string{"cpu=1%"})
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{
		Repository: makeRepository(t), ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: dir, TestingUnsafeDisableIsolation: true, Tradeoff: speed,
	})
	require.NoError(t, err)
	state := engine.State()
	require.Equal(t, speed, state.Tradeoff)
	require.NoError(t, engine.Close())

	resumed, err := Resume(dir, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, resumed.Close()) }()
	require.Equal(t, speed, resumed.State().Tradeoff)
	require.Equal(t, state.Manifest.Performance, resumed.State().Manifest.Performance)

	report := RenderMarkdown(resumed.State())
	require.Contains(t, report, "- Judged under: trade-off `speed` with overrides: improve wall_time_ns by at least 3%; may regress at most")
	require.Contains(t, report, "cpu_time_ns +1%")
	require.Contains(t, report, "--tradeoff speed --allow cpu_time_ns=1%")
}
