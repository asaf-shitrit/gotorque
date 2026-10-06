package campaign

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// TestStoppedDiscoveryIsNotMarkedComplete: the discovery step is the one a
// resume skips once it is marked done, so a campaign stopped inside it must
// leave it undone.
func TestStoppedDiscoveryIsNotMarkedComplete(t *testing.T) {
	engine := discoveryEngine(t, profile.NewReplay())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, engine.runDiscoveryStep(ctx), context.Canceled)
	require.False(t, engine.State().CompletedSteps["discovery_profile"])
	require.Empty(t, engine.State().DiscoveryProfileSource)
}
