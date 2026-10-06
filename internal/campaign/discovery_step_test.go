package campaign

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/discovery"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// recordedTranscript loads a sampler transcript recorded from a real run.
func recordedTranscript(t *testing.T, name string) profile.Transcript {
	t.Helper()
	transcript, err := profile.LoadTranscript(filepath.Join("..", "profile", "testdata", "transcripts", name+".json"))
	require.NoError(t, err)
	return transcript
}

func discoveryEngine(t *testing.T, sampler profile.Sampler) *Engine {
	t.Helper()
	engine, err := Create(context.Background(), Options{
		Repository: makeRepository(t), ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	engine.sampler = sampler
	return engine
}

func eventsOf(t *testing.T, engine *Engine) map[string]Event {
	t.Helper()
	events, err := engine.store.Events()
	require.NoError(t, err)
	byType := map[string]Event{}
	for _, event := range events {
		byType[event.Type] = event
	}
	return byType
}

// TestDiscoveryStepAppliesEvidenceToTheStateResumeReads runs a whole baseline
// discovery against a scripted sampler: the evidence lands in the persisted
// State fields (unchanged, so a campaign saved by an older build resumes), and
// the completed event counts what was applied.
func TestDiscoveryStepAppliesEvidenceToTheStateResumeReads(t *testing.T) {
	busy := recordedTranscript(t, "macos-busy")
	busy.IsolationNotes = []string{"max_memory_bytes is not enforced while sampling"}
	replay := profile.NewReplay(busy)
	engine := discoveryEngine(t, replay)
	require.NoError(t, engine.Run(context.Background()))

	state := engine.State()
	require.True(t, state.CompletedSteps["discovery_profile"])
	require.Equal(t, discovery.SourceTargetSample, state.DiscoveryProfileSource)
	require.NotEmpty(t, state.DiscoveryHotFunctions)
	require.Contains(t, state.DiscoveryHotFunctionWeights, "main.hot")
	require.FileExists(t, state.DiscoveryProfileSummaryPath)
	require.Contains(t, state.SandboxIsolationNotes, "max_memory_bytes is not enforced while sampling")
	require.Len(t, replay.Requests(), 1)

	completed := eventsOf(t, engine)["discovery_profile_completed"]
	require.Equal(t, "measured "+strconv.Itoa(len(state.DiscoveryHotFunctions))+" hot functions from a target sample", completed.Message)

	require.NoError(t, engine.Close())
	resumed, err := Resume(engine.dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Close() })
	require.Equal(t, state.DiscoveryHotFunctions, resumed.State().DiscoveryHotFunctions)
	require.Equal(t, state.DiscoveryProfileSource, resumed.State().DiscoveryProfileSource)
}

// TestDiscoveryStepSavesWhyEveryProfileFailed: a sampler that cannot sample
// and a module with no benchmarks leave no hot list, and the events say so.
func TestDiscoveryStepSavesWhyEveryProfileFailed(t *testing.T) {
	engine := discoveryEngine(t, profile.NewReplay(profile.Transcript{Sampler: profile.SamplerMacOS, ExitedBeforeAttach: true}))
	require.NoError(t, engine.Run(context.Background()))

	state := engine.State()
	require.Equal(t, discovery.SourceNone, state.DiscoveryProfileSource)
	require.Empty(t, state.DiscoveryHotFunctions)
	events := eventsOf(t, engine)
	require.Contains(t, events["discovery_sample_failed"].Message, "target exited before sampling began")
	require.Contains(t, events["discovery_profile_skipped"].Message, "benchmark CPU profile unavailable")
	require.Equal(t, "measured 0 hot functions from no source", events["discovery_profile_completed"].Message)
}
