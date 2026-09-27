package campaign

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
)

// writePastCampaign persists a finished campaign's state the way the engine
// does, so loadHistory reads it back through LoadReport like a real one.
func writePastCampaign(t *testing.T, id, revision string, records []CandidateRecord) string {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	state := State{ID: id, Environment: Environment{Revision: revision}, CandidateRecords: records,
		Manifest: manifest.Manifest{Campaign: manifest.CampaignLimits{
			MaxDuration:           manifest.Duration(time.Minute),
			DiscoveryStallTimeout: manifest.Duration(time.Minute),
			MinimumCommandTimeout: manifest.Duration(time.Second),
		}}}
	require.NoError(t, store.Save(state))
	require.NoError(t, store.Close())
	return dir
}

func measured(target agents.Target) CandidateRecord {
	return CandidateRecord{Target: &target, Comparisons: []domain.MetricComparison{{Metric: "wall_time_ns", Workload: "wl", Unit: "ns", Baseline: 1, Candidate: 1}}}
}

var (
	targetExec   = agents.Target{Function: "WithExecutorID", Location: "execution/context.go:16", Cause: "string_build"}
	targetUnpack = agents.Target{Function: "(*Value).UnpackKinds", Location: "model/value.go:187", Cause: "throwaway_result"}
	targetNewPtr = agents.Target{Function: "newPtr", Location: "model/value_literal.go:8", Cause: "redundant"}
)

func TestLoadHistoryCarriesMeasuredTargetsOnly(t *testing.T) {
	dir := writePastCampaign(t, "past-1", "rev-a", []CandidateRecord{
		measured(targetExec),
		{Target: &targetNewPtr, Decision: domain.DecisionRejected}, // rejected before measurement
		{Target: &targetUnpack, Accepted: true},
		measured(targetExec), // the same target again is carried once
		{Hypothesis: "untargeted"},
	})
	targets, sources, err := loadHistory([]string{dir}, "rev-a")
	require.NoError(t, err)
	require.Equal(t, []agents.Target{targetExec, targetUnpack}, targets)
	require.Len(t, sources, 1)
	require.Equal(t, "past-1", sources[0].CampaignID)
	require.Equal(t, 2, sources[0].Targets)
	require.Empty(t, sources[0].Skipped)
}

func TestLoadHistorySkipsAnotherRevision(t *testing.T) {
	dir := writePastCampaign(t, "past-2", "0123456789abcdef0123", []CandidateRecord{measured(targetExec)})
	targets, sources, err := loadHistory([]string{dir}, "fedcba9876543210fedc")
	require.NoError(t, err)
	require.Empty(t, targets)
	require.Contains(t, sources[0].Skipped, "0123456789ab")
	require.Contains(t, sources[0].Skipped, "fedcba987654")
}

func TestLoadHistoryDeduplicatesAcrossCampaigns(t *testing.T) {
	first := writePastCampaign(t, "p1", "rev", []CandidateRecord{measured(targetExec)})
	second := writePastCampaign(t, "p2", "rev", []CandidateRecord{measured(targetExec), measured(targetNewPtr)})
	targets, sources, err := loadHistory([]string{first, second}, "rev")
	require.NoError(t, err)
	require.Equal(t, []agents.Target{targetExec, targetNewPtr}, targets)
	require.Equal(t, 1, sources[0].Targets)
	require.Equal(t, 1, sources[1].Targets)
}

func TestLoadHistoryRejectsAnUnreadableDirectory(t *testing.T) {
	_, _, err := loadHistory([]string{filepath.Join(t.TempDir(), "missing")}, "rev")
	require.ErrorContains(t, err, "read --history")
}

func TestPriorTargetsIncludeHistory(t *testing.T) {
	e := &Engine{state: State{HistoryTargets: []agents.Target{targetExec}, CandidateRecords: []CandidateRecord{measured(targetNewPtr)}}}
	require.Equal(t, []agents.Target{targetExec, targetNewPtr}, e.priorTargets())
}

func TestReportListsHistory(t *testing.T) {
	state := State{
		HistoryTargets: []agents.Target{targetExec},
		HistorySources: []HistorySource{
			{Directory: "/w/c1", CampaignID: "c1", Targets: 1},
			{Directory: "/w/c2", CampaignID: "c2", Skipped: "revision aaa, not bbb"},
		},
	}
	var b strings.Builder
	writeHistory(&b, state)
	out := b.String()
	require.Contains(t, out, "## Campaign history")
	require.Contains(t, out, "| `c1` | `/w/c1` | 1 |")
	require.Contains(t, out, "skipped: revision aaa, not bbb")
	require.Contains(t, out, "`WithExecutorID` at `execution/context.go:16`, string_build")
	require.Equal(t, ` --history "/w/c1" --history "/w/c2"`, historyFlags(state.HistorySources))

	var empty strings.Builder
	writeHistory(&empty, State{})
	require.Empty(t, empty.String())
}

func TestShortRevision(t *testing.T) {
	require.Equal(t, "unknown", shortRevision(""))
	require.Equal(t, "abc", shortRevision("abc"))
	require.Equal(t, "0123456789ab", shortRevision("0123456789abcdef"))
}
