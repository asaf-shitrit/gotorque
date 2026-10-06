package campaign

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/discovery"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// TestDiscoveryDoesNotCallAnIdleSampleAProfile replays the first pup campaign:
// every sample caught only parked threads. It used to end as "measured 0 hot
// functions from a target sample", and the analysis then flagged nothing. Now
// the sample counts as failed, the event says the target was idle, and the
// campaign tries the next source.
func TestDiscoveryDoesNotCallAnIdleSampleAProfile(t *testing.T) {
	engine := discoveryEngine(t, profile.NewReplay(recordedTranscript(t, "macos-pup-idle")))
	require.NoError(t, engine.Run(context.Background()))

	require.NotEqual(t, discovery.SourceTargetSample, engine.State().DiscoveryProfileSource)
	events := eventsOf(t, engine)
	require.Contains(t, events["discovery_sample_failed"].Message, "caught the target idle")
	require.NotContains(t, events["discovery_profile_completed"].Message, "from a target sample")
}
