package discovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// TestAnIdleSampleClimbsTheLadder: pup's amplified stdin was still being read
// when the sampler looked, so it caught only parked threads. That is a sample
// taken too early, not a profile of pup: it is taken again at the same size
// (profile.Sample), then on a larger input, then on the next stress seed,
// before the benchmark fallback is considered.
func TestAnIdleSampleClimbsTheLadder(t *testing.T) {
	idle := recorded(t, "macos-pup-idle")
	replay := profile.NewReplay(idle, idle, idle, idle, recordedBusy(t))
	repeatable := manifest.SeedWorkload{ID: "select", Tier: domain.TierRepresentative, Files: []manifest.FixtureFile{{Path: "page.html", Content: "<p>x</p>\n", Repeat: 10}}}
	stress := manifest.SeedWorkload{ID: "select-huge", Tier: domain.TierStress, Args: []string{"huge"}}
	seed, result, err := newLadder(t, replay, repeatable, stress).FirstLiving(context.Background())
	require.NoError(t, err)
	require.Equal(t, "select-huge", seed.ID)
	require.NotEmpty(t, result.Functions)

	requests := replay.Requests()
	require.Len(t, requests, 5)
	require.Equal(t, requests[0].Fixtures["page.html"], requests[1].Fixtures["page.html"], "the same input once more")
	require.Greater(t, len(requests[2].Fixtures["page.html"]), 4*len(requests[0].Fixtures["page.html"]), "then a larger one")
	require.Equal(t, requests[2].Fixtures["page.html"], requests[3].Fixtures["page.html"])
	require.Contains(t, requests[4].Args, "huge", "then the next stress seed")
}

// TestRunFallsBackToBenchmarksWhenEverySampleCaughtTheTargetIdle: the event
// says why, instead of "measured 0 hot functions from a target sample".
func TestRunFallsBackToBenchmarksWhenEverySampleCaughtTheTargetIdle(t *testing.T) {
	in := runInputs(t, moduleRepo(t, true))
	ev, err := Run(context.Background(), in, profile.NewReplay(recorded(t, "macos-pup-idle")), testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceBenchmark, ev.Source)
	require.Equal(t, []string{"discovery_sample_failed"}, kinds(ev.Events))
	require.Contains(t, ev.Events[0].Message, "caught the target idle")
	require.NotEmpty(t, ev.HotFunctions)
}
