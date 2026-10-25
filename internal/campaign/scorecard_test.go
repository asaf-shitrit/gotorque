package campaign

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
)

func TestVerdictClass(t *testing.T) {
	cases := map[string]CandidateRecord{
		classHarness:      {Accepted: true, LoadContended: true},
		classAccepted:     {Accepted: true, Decision: domain.DecisionAccepted},
		classInconclusive: {Decision: domain.DecisionInconclusive},
		classTests:        {Decision: domain.DecisionRejected, Summary: "upstream test suite failed: new failing tests: x"},
		classBuild:        {Decision: domain.DecisionRejected, Summary: "candidate build failed: go build"},
		classGuardrail:    {Decision: domain.DecisionRejected, Reasons: []string{`workload "w" regressed by +2.5%, over the 2.00% limit`}},
		classOther:        {Decision: domain.DecisionRejected, Summary: "measurement failed on workload"},
	}
	for want, record := range cases {
		require.Equal(t, want, verdictClass(record), "%+v", record)
	}
	require.Equal(t, classHarness, verdictClass(CandidateRecord{Summary: "candidate rejected before build: the optimizer did not answer: timeout"}))
	require.Equal(t, classBuild, verdictClass(CandidateRecord{Summary: "candidate rejected before build: patch shape"}))
}

func TestScorecardCountsVerdictsAndVerifications(t *testing.T) {
	state := State{ID: "c", StopReason: "maximum candidate count reached", CandidateRecords: []CandidateRecord{
		{Attempt: 1, Accepted: true, Decision: domain.DecisionAccepted},
		{Attempt: 2, Accepted: true, Decision: domain.DecisionAccepted},
		{Attempt: 3, Decision: domain.DecisionInconclusive},
	}, Verifications: []Verification{
		{Attempt: 1, Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted},
		{Attempt: 2, Original: domain.DecisionAccepted, Decision: domain.DecisionInconclusive},
	}}
	state.Manifest.Name = "tool CLI"
	row := scorecardRow("camp", state)
	require.Equal(t, 2, row.Counts[classAccepted])
	require.Equal(t, 1, row.Counts[classInconclusive])
	require.Equal(t, 2, row.Verified)
	require.Equal(t, 1, row.Held)

	out := RenderScorecard([]ScorecardRow{row, row})
	require.Contains(t, out, "| camp | tool CLI | unknown | 2 | 1 | 0 | 0 | 0 | 0 | 0 | 2/1 | maximum candidate count reached |")
	require.Contains(t, out, "| **total** | 2 campaigns |  | 4 | 2 | 0 | 0 | 0 | 0 | 0 | 4/2 |")
	require.Equal(t, 5, strings.Count(out, "\n"), "header, separator, two rows and the total")
}

func TestScorecardReadsCampaignDirectories(t *testing.T) {
	dir := writePastCampaign(t, "past", "rev", []CandidateRecord{{Attempt: 1, Decision: domain.DecisionInconclusive}})
	rows, err := Scorecard([]string{dir})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Counts[classInconclusive])
	_, err = Scorecard([]string{filepath.Join(t.TempDir(), "missing")})
	require.Error(t, err)
}

func TestScorecardSeparatesNullCampaigns(t *testing.T) {
	agentState := State{ID: "agent", CandidateRecords: []CandidateRecord{
		{Attempt: 1, Accepted: true, Decision: domain.DecisionAccepted},
		{Attempt: 2, Decision: domain.DecisionInconclusive},
	}, Verifications: []Verification{{Attempt: 1, Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted}}}
	agentState.Manifest.Name = "tool CLI"
	nullState := State{ID: "null", NullCandidates: 4, CandidateRecords: []CandidateRecord{
		{Attempt: 1, Accepted: true, Decision: domain.DecisionAccepted},
		{Attempt: 2, Decision: domain.DecisionInconclusive},
		{Attempt: 3, Decision: domain.DecisionRejected, Summary: "candidate build failed: go build"},
		{Attempt: 4, Accepted: true, LoadContended: true, Decision: domain.DecisionAccepted},
	}}
	nullState.Manifest.Name = "tool CLI"

	rows := []ScorecardRow{scorecardRow("agent", agentState), scorecardRow("null", nullState)}
	require.Equal(t, 0, rows[0].NullCandidates)
	require.Equal(t, 4, rows[1].NullCandidates)

	out := RenderScorecard(rows)
	require.Contains(t, out, "## Agent campaigns")
	// The agent totals hold the agent campaign only: the null accept and its
	// contended harness verdict stay out of the accepted and verified columns.
	require.Contains(t, out, "| **total** | 1 campaigns |  | 1 | 1 | 0 | 0 | 0 | 0 | 0 | 1/1 |")
	require.Contains(t, out, "## Null candidates")
	// Valid runs exclude the harness verdict, so 3 of the 4 records count; one
	// accept and one build rejection give a 33.33% rate each.
	require.Contains(t, out, "| null | tool CLI | unknown | 3 | 1 | 1 | 33.33% | 86.46% | 33.33% | 86.46% |")
	require.Contains(t, out, "| **total** | 1 campaigns |  | 3 | 1 | 1 | 33.33% | 86.46% | 33.33% | 86.46% |")
	require.Less(t, strings.Index(out, "## Agent campaigns"), strings.Index(out, "## Null candidates"))
}

func TestScorecardHasNoNullSectionWithoutNullCampaigns(t *testing.T) {
	state := State{ID: "agent", CandidateRecords: []CandidateRecord{{Attempt: 1, Decision: domain.DecisionInconclusive}}}
	state.Manifest.Name = "tool CLI"
	out := RenderScorecard([]ScorecardRow{scorecardRow("agent", state)})
	require.NotContains(t, out, "## Null candidates")
	require.NotContains(t, out, "## Agent campaigns")
}

func TestNullScorecardBoundsZeroAndKOfN(t *testing.T) {
	state := State{ID: "null", NullCandidates: 70, CandidateRecords: make([]CandidateRecord, 0, 70)}
	for i := range 70 {
		state.CandidateRecords = append(state.CandidateRecords, CandidateRecord{Attempt: i + 1, Decision: domain.DecisionInconclusive})
	}
	state.Manifest.Name = "tool CLI"
	row := scorecardRow("quiet", state)
	require.Equal(t, nullScore{Valid: 70}, row.nullScore())

	out := RenderScorecard([]ScorecardRow{row})
	// 0 of 70: the exact bound is 1 - 0.05^(1/70), about 3/n.
	require.Contains(t, out, "| 70 | 0 | 0 | 0.00% | 4.19% | 0.00% | 4.19% |")
	require.InDelta(t, 1-math.Pow(0.05, 1.0/70), clopperPearsonUpper(0, 70), 1e-12)

	kOfN := State{ID: "k", NullCandidates: 50, CandidateRecords: make([]CandidateRecord, 0, 50)}
	for i := range 50 {
		record := CandidateRecord{Attempt: i + 1, Decision: domain.DecisionInconclusive, Summary: "measurement failed on workload"}
		if i < 3 {
			record = CandidateRecord{Attempt: i + 1, Accepted: true, Decision: domain.DecisionAccepted}
		}
		kOfN.CandidateRecords = append(kOfN.CandidateRecords, record)
	}
	// An unclassified rejection is neither a false accept nor a false reject,
	// but it does count in the valid runs and so lowers the rates.
	require.Equal(t, nullScore{Valid: 50, FalseAccepts: 3}, scorecardRow("k", kOfN).nullScore())
	out = RenderScorecard([]ScorecardRow{scorecardRow("k", kOfN)})
	require.Contains(t, out, "| 50 | 3 | 0 | 6.00% | 14.78% | 0.00% | 5.82% |")
}

func TestScorecardReadsNullCandidatesFromDisk(t *testing.T) {
	agentDir := writeScorecardCampaign(t, State{ID: "agent", CandidateRecords: []CandidateRecord{{Attempt: 1, Decision: domain.DecisionInconclusive}}})
	nullDir := writeScorecardCampaign(t, State{ID: "null", NullCandidates: 12, CandidateRecords: []CandidateRecord{{Attempt: 1, Decision: domain.DecisionInconclusive}}})
	rows, err := Scorecard([]string{agentDir, nullDir})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, 0, rows[0].NullCandidates)
	require.Equal(t, 12, rows[1].NullCandidates)
}

func TestRateCellWithoutRuns(t *testing.T) {
	rate, upper := rateCell(0, 0)
	require.Equal(t, "n/a", rate)
	require.Equal(t, "n/a", upper)
	rate, upper = rateCell(1, 4)
	require.Equal(t, "25.00%", rate)
	require.Equal(t, percent(clopperPearsonUpper(1, 4)), upper)
}

func writeScorecardCampaign(t *testing.T, state State) string {
	t.Helper()
	dir := t.TempDir()
	state.Manifest = manifest.Manifest{Campaign: manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}}
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	require.NoError(t, store.Save(state))
	require.NoError(t, store.Close())
	return dir
}
