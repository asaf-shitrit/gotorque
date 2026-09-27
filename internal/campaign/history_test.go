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
	"example.com/gotorque/internal/orchestrator"
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
	h, err := loadHistory([]string{dir}, "rev-a")
	require.NoError(t, err)
	require.Equal(t, []agents.Target{targetExec, targetUnpack}, h.Targets)
	require.Len(t, h.Sources, 1)
	require.Equal(t, "past-1", h.Sources[0].CampaignID)
	require.Equal(t, 2, h.Sources[0].Targets)
	require.Empty(t, h.Sources[0].Skipped)
}

func TestLoadHistorySkipsAnotherRevision(t *testing.T) {
	dir := writePastCampaign(t, "past-2", "0123456789abcdef0123", []CandidateRecord{measured(targetExec)})
	h, err := loadHistory([]string{dir}, "fedcba9876543210fedc")
	require.NoError(t, err)
	require.Empty(t, h.Targets)
	require.Empty(t, h.Candidates)
	require.Contains(t, h.Sources[0].Skipped, "0123456789ab")
	require.Contains(t, h.Sources[0].Skipped, "fedcba987654")
}

func TestLoadHistoryDeduplicatesAcrossCampaigns(t *testing.T) {
	first := writePastCampaign(t, "p1", "rev", []CandidateRecord{measured(targetExec)})
	second := writePastCampaign(t, "p2", "rev", []CandidateRecord{measured(targetExec), measured(targetNewPtr)})
	h, err := loadHistory([]string{first, second}, "rev")
	require.NoError(t, err)
	require.Equal(t, []agents.Target{targetExec, targetNewPtr}, h.Targets)
	require.Equal(t, 1, h.Sources[0].Targets)
	require.Equal(t, 1, h.Sources[1].Targets)
}

func TestLoadHistoryRejectsAnUnreadableDirectory(t *testing.T) {
	_, err := loadHistory([]string{filepath.Join(t.TempDir(), "missing")}, "rev")
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

func TestLoadHistoryCarriesMeasuredCandidateIDs(t *testing.T) {
	inconclusive := measured(targetExec)
	inconclusive.CandidateID, inconclusive.Attempt, inconclusive.Decision = "c-measured", 2, domain.DecisionInconclusive
	unmeasured := CandidateRecord{CandidateID: "c-unmeasured", Target: &targetNewPtr}
	dir := writePastCampaign(t, "past-3", "rev", []CandidateRecord{inconclusive, unmeasured})
	h, err := loadHistory([]string{dir}, "rev")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"c-measured": "campaign past-3 attempt 2: inconclusive"}, h.Candidates)
}

func TestRejectMeasuredDuplicate(t *testing.T) {
	own := measured(targetNewPtr)
	own.CandidateID, own.Attempt, own.Decision = "c-own", 1, domain.DecisionInconclusive
	e := &Engine{state: State{
		HistoryCandidates: map[string]string{"c-past": "campaign p attempt 3: inconclusive"},
		CandidateRecords:  []CandidateRecord{own, {CandidateID: "c-rejected", Decision: domain.DecisionRejected}},
	}}

	var fromHistory orchestrator.CandidateEvidence
	require.True(t, e.rejectMeasuredDuplicate("c-past", &fromHistory))
	require.True(t, fromHistory.Unmeasured)
	require.Contains(t, fromHistory.Summary, "identical patch was already measured")
	require.Contains(t, fromHistory.Summary, "campaign p attempt 3")

	var fromThisCampaign orchestrator.CandidateEvidence
	require.True(t, e.rejectMeasuredDuplicate("c-own", &fromThisCampaign))
	require.Contains(t, fromThisCampaign.FailureDetail, "attempt 1 of this campaign: inconclusive")

	var fresh orchestrator.CandidateEvidence
	require.False(t, e.rejectMeasuredDuplicate("c-new", &fresh))
	require.False(t, e.rejectMeasuredDuplicate("c-rejected", &fresh), "a candidate rejected before measurement proves nothing about its patch")
	require.Empty(t, fresh.Summary)
}

// TestCandidateMetaNamesEachTransport pins the report line for both
// code-built transports; function_sources records rendered no transport line.
func TestCandidateMetaNamesEachTransport(t *testing.T) {
	for transport, want := range map[string]string{
		FunctionSourceTransport:      "- Transport: function_source (",
		MultiFunctionSourceTransport: "- Transport: function_sources (",
		PatchTransport:               "",
	} {
		var b strings.Builder
		writeCandidateMeta(&b, CandidateRecord{Transport: transport})
		if want == "" {
			require.NotContains(t, b.String(), "Transport", transport)
			continue
		}
		require.Contains(t, b.String(), want, transport)
	}
}
