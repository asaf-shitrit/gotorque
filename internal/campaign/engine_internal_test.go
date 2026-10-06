package campaign

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
)

// A campaign stopped by its duration budget never reaches finishCampaign, so
// the only thing on disk used to be a mid-run snapshot: gron-9 spent its full
// ninety minutes and its report still said "running" with no stop reason,
// while the database held `max_duration 1h30m0s spent`.
func TestStoppedCampaignWritesItsTerminalReport(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := &Engine{dir: dir, store: store, progress: io.Discard, now: func() time.Time { return time.Now().UTC() }}
	engine.state = State{
		ID: "campaign-stopped", Status: StatusRunning,
		Manifest:         manifest.Manifest{Campaign: manifest.CampaignLimits{MaxDuration: manifest.Duration(90 * time.Minute), DiscoveryStallTimeout: manifest.Duration(time.Minute), MinimumCommandTimeout: manifest.Duration(time.Second)}},
		CandidateRecords: []CandidateRecord{{Attempt: 1, CandidateID: "cand-1", Decision: domain.DecisionInconclusive}},
		TokenUsage:       map[string]RoleUsageSnapshot{"optimizer": {Requests: 3, TotalTokens: 4321}},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrDurationBudgetExhausted)
	runErr := errors.New("stopped mid-flight")

	engine.captureRunFailure(ctx, &runErr)

	state, err := LoadReport(dir)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, state.Status)
	require.Contains(t, state.StopReason, "max_duration")
	require.Len(t, state.CandidateRecords, 1, "the report must carry the candidates that were evaluated")
	require.Equal(t, int64(4321), state.TokenUsage["optimizer"].TotalTokens, "cost accounting must survive a spent budget")
}
