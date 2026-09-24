package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/manifest"
)

// TestTradeoffFlagsFailBeforeAnyProviderIsCalled: a wrong metric or preset
// ends the command before --adk spends a preflight request, and a resumed
// campaign keeps the trade-off it started with.
func TestTradeoffFlagsFailBeforeAnyProviderIsCalled(t *testing.T) {
	for want, f := range map[string]optimizeFlags{
		"unknown trade-off": {repo: "r", manifestPath: "m", runADK: true, tradeoffName: "fastest"},
		"unknown metric":    {repo: "r", manifestPath: "m", runADK: true, allow: []string{"latency=5%"}},
		"cannot allow":      {repo: "r", manifestPath: "m", runADK: true, tradeoffName: "lean", allow: []string{"memory=1%"}},
	} {
		require.ErrorContains(t, runOptimize(context.Background(), &bytes.Buffer{}, f), want)
	}
	err := runOptimize(context.Background(), &bytes.Buffer{}, optimizeFlags{resume: t.TempDir(), runADKStub: true, tradeoffName: "speed"})
	require.ErrorIs(t, err, manifest.ErrTradeoffOnResume)
}

func TestTheOptimizeCommandOffersTradeoffFlags(t *testing.T) {
	cmd := newOptimizeCommand(&bytes.Buffer{})
	require.NotNil(t, cmd.Flags().Lookup("tradeoff"))
	require.NoError(t, cmd.Flags().Set("allow", "memory=5%"))
	require.NoError(t, cmd.Flags().Set("allow", "cpu=1%"))
	allow, err := cmd.Flags().GetStringArray("allow")
	require.NoError(t, err)
	require.Equal(t, []string{"memory=5%", "cpu=1%"}, allow)
}

func TestACampaignAnnouncesItsTradeoff(t *testing.T) {
	performance := manifest.PerformancePolicy{PrimaryMetric: "peak_memory_bytes", MinimumImprovementPercent: 3, Guardrails: []manifest.Guardrail{{Name: "wall_time_ns", MaximumRegressionPercent: 3}}}
	var out bytes.Buffer
	require.NoError(t, announceTradeoff(&out, manifest.Tradeoff{}, performance))
	require.Empty(t, out.String(), "the manifest as written needs no announcement")
	require.NoError(t, announceTradeoff(&out, manifest.Presets["lean"], performance))
	require.Equal(t, "judged under: improve peak_memory_bytes by at least 3%; may regress at most wall_time_ns +3%\n", out.String())
}

// TestCreateAndRunOptimizeStopsBeforeACampaignExists covers the two ways a
// fresh run ends before anything is built: no repository or manifest, and a
// manifest that does not load.
func TestCreateAndRunOptimizeStopsBeforeACampaignExists(t *testing.T) {
	require.ErrorContains(t, createAndRunOptimize(context.Background(), &bytes.Buffer{}, optimizeFlags{}, nil, nil), "--repo and --manifest are required")
	err := createAndRunOptimize(context.Background(), &bytes.Buffer{}, optimizeFlags{repo: t.TempDir(), manifestPath: filepath.Join(t.TempDir(), "missing.json")}, nil, nil)
	require.Error(t, err)
}
