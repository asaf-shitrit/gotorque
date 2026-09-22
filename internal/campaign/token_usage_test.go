package campaign

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/manifest"
	"google.golang.org/genai"
)

// TestRecordTokenUsageAccumulatesAcrossProcesses pins the fix for
// under-reported token spend: recordTokenUsage used to replace
// state.TokenUsage outright with the current process's collector snapshot,
// so a resumed campaign's report reflected only the last process's spend.
func TestRecordTokenUsageAccumulatesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	m, err := manifest.LoadFile(writeManifest(t, t.TempDir()))
	require.NoError(t, err)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }

	first, err := compose(dir, store, State{Manifest: m, CompletedSteps: map[string]bool{}}, nil, tick)
	require.NoError(t, err)
	firstUsage := agents.NewUsageCollector()
	firstUsage.Record("optimizer", usageMetadata(100, 50))
	first.recordTokenUsage(agents.Set{Usage: firstUsage})
	require.Equal(t, int64(150), first.state.TokenUsage["optimizer"].TotalTokens)
	require.NoError(t, first.saveEvent("test_event", "first process spent tokens", nil))

	persisted, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, int64(150), persisted.TokenUsage["optimizer"].TotalTokens)

	// A new process resumes with a fresh collector, as NewOpenAIProviderFromEnvironment
	// would create. Its snapshot alone reports only this process's spend, but
	// recordTokenUsage must add it on top of what the first process persisted.
	second, err := compose(dir, store, persisted, nil, tick)
	require.NoError(t, err)
	secondUsage := agents.NewUsageCollector()
	secondUsage.Record("optimizer", usageMetadata(30, 10))
	second.recordTokenUsage(agents.Set{Usage: secondUsage})
	require.Equal(t, int64(190), second.state.TokenUsage["optimizer"].TotalTokens, "second process's spend must add to, not replace, the first process's")
	require.Equal(t, int64(2), second.state.TokenUsage["optimizer"].Requests)

	// Calling it again in the same process (as RunADK's deferred call and a
	// later saveEvent refresh both would) must not double the total: the
	// collector's snapshot is already cumulative for this process, and the
	// baseline captured on the first call never moves.
	second.recordTokenUsage(agents.Set{Usage: secondUsage})
	require.Equal(t, int64(190), second.state.TokenUsage["optimizer"].TotalTokens, "repeated calls in one process must not double-count")
}

// TestSaveEventRefreshesTokenUsageWhileADKIsActive pins the fix for a
// SIGKILLed process losing its spend: token usage used to be recorded only in
// RunADK's deferred call, which never runs on a hard kill. saveEvent must
// keep the persisted snapshot current whenever an ADK run is active.
func TestSaveEventRefreshesTokenUsageWhileADKIsActive(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	m, err := manifest.LoadFile(writeManifest(t, t.TempDir()))
	require.NoError(t, err)

	engine, err := compose(dir, store, State{Manifest: m, CompletedSteps: map[string]bool{}}, nil, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)

	usage := agents.NewUsageCollector()
	roleSet := &agents.Set{Usage: usage}
	engine.SetADK(roleSet, nil)

	usage.Record("optimizer", usageMetadata(20, 5))
	require.NoError(t, engine.saveEvent("adk_progress", "mid-run", nil))

	persisted, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, int64(25), persisted.TokenUsage["optimizer"].TotalTokens, "saveEvent must persist spend before RunADK returns, so a killed process does not lose it")
}

func usageMetadata(prompt, completion int32) *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: prompt, CandidatesTokenCount: completion, TotalTokenCount: prompt + completion}
}
