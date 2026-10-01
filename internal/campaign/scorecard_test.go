package campaign

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
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
	require.Contains(t, out, "| camp | tool CLI | 2 | 1 | 0 | 0 | 0 | 0 | 0 | 2/1 | maximum candidate count reached |")
	require.Contains(t, out, "| **total** | 2 campaigns | 4 | 2 | 0 | 0 | 0 | 0 | 0 | 4/2 |")
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
