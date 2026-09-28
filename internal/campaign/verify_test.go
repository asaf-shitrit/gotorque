package campaign

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
)

// TestVerifyReevaluatesARecordedCandidate: an accepted candidate is evaluated
// again from its recorded patch, with the requested pair count, and the
// verification is persisted and reported. The duplicate refusal, which would
// otherwise stop a patch this campaign already measured, is off.
func TestVerifyReevaluatesARecordedCandidate(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Target:   target,
		Proposal: agents.OptimizerResult{Hypothesis: "buffer stdout", FunctionSource: "func main() {\n\tb, _ := os.ReadFile(\"fixture.txt\")\n\tw := bufio.NewWriter(os.Stdout)\n\tdefer w.Flush()\n\tfmt.Fprintf(w, \"%s\", b)\n}", Imports: []string{"bufio"}},
	})
	require.NoError(t, err)
	engine.state.CandidateRecords = append(engine.state.CandidateRecords, CandidateRecord{
		Attempt: 1, CandidateID: evidence.Candidate.ID, Target: target, PatchPath: evidence.Candidate.PatchPath,
		Decision: domain.DecisionAccepted, Accepted: true, Comparisons: evidence.Comparisons,
	})
	require.Equal(t, []int{1}, engine.AcceptedAttempts())

	v, err := engine.Verify(context.Background(), 1, 2)
	require.NoError(t, err)
	require.Equal(t, 1, v.Attempt)
	require.Equal(t, 2, v.Pairs)
	require.Equal(t, domain.DecisionAccepted, v.Original)
	require.NotEmpty(t, v.Decision)
	require.NotContains(t, v.Summary, "identical patch was already measured")
	require.Len(t, engine.State().Verifications, 1)
	require.Zero(t, engine.pairs()-measurementRepetitions, "the override ends with the verification")

	var b strings.Builder
	writeVerifications(&b, engine.State())
	require.Contains(t, b.String(), "## Verification")
	require.Contains(t, b.String(), "| 1 | 2 | accepted |")
}

func TestVerifyRefusesAnUnknownOrPatchlessAttempt(t *testing.T) {
	e := &Engine{state: State{CandidateRecords: []CandidateRecord{{Attempt: 2}}}}
	_, err := e.Verify(context.Background(), 7, 2)
	require.ErrorContains(t, err, "no attempt 7")
	_, err = e.Verify(context.Background(), 2, 2)
	require.ErrorContains(t, err, "no recorded patch")
	require.Empty(t, e.AcceptedAttempts())
}

func TestVerificationConfirmed(t *testing.T) {
	require.True(t, Verification{Original: domain.DecisionAccepted, Decision: domain.DecisionAccepted}.Confirmed())
	require.False(t, Verification{Original: domain.DecisionAccepted, Decision: domain.DecisionInconclusive}.Confirmed())
	var b strings.Builder
	writeVerifications(&b, State{})
	require.Empty(t, b.String())
	writeVerifications(&b, State{Verifications: []Verification{{Attempt: 3, Pairs: 60, Original: domain.DecisionAccepted, Decision: domain.DecisionInconclusive, LoadAverages: []float64{9.5}, LoadContended: true}}})
	require.Contains(t, b.String(), "| 3 | 60 | accepted | inconclusive | 9.50 (contended) | no |")
}
