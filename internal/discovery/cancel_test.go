package discovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// TestRunReportsACancelledContextInsteadOfNoEvidence: a campaign stopped during
// discovery used to come out of it with "no source", which the engine recorded
// as a finished step, so a resume never discovered anything.
func TestRunReportsACancelledContextInsteadOfNoEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := Run(ctx, runInputs(t, moduleRepo(t, false)), profile.NewReplay(gone), testToolchain)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, ev.Events, "a stopped discovery has no evidence to apply")
}
