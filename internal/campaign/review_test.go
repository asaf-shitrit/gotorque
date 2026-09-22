package campaign

import (
	"context"
	"errors"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

const writePatch = `--- a/fixture.go
+++ b/fixture.go
@@ -11,6 +11,8 @@ func write(w io.Writer, records []string) {
 // write prints every record on its own write call.
 func write(w io.Writer, records []string) {
+	bw := bufio.NewWriter(w)
+	defer bw.Flush()
 	for _, r := range records {
-		fmt.Fprintln(w, r)
+		fmt.Fprintln(bw, r)
 	}
`

// hazardEvaluator answers the review questions: a probability per hazard, and
// Jev's usual answer, well below one half, for the rest.
type hazardEvaluator struct {
	answers map[jev.Hazard]float64
	state   map[string]string
	err     error
}

func (h *hazardEvaluator) Evaluate(_ context.Context, req jev.Request) (jev.Response, error) {
	h.state, _ = req.State.(map[string]string)
	answers := map[string]jev.Answer{}
	for _, hz := range jev.Hazards {
		answers[string(hz)] = jev.Answer{Type: "boolean", Probability: 0.05}
	}
	for hz, p := range h.answers {
		answers[string(hz)] = jev.Answer{Type: "boolean", Probability: p}
	}
	return jev.Response{Answers: answers, Usage: jev.Usage{InputTokens: 700, OutputTokens: 60}}, h.err
}

func TestReviewRaisesTheDroppedFlushError(t *testing.T) {
	repo := writeCauseFixture(t)
	evaluator := &hazardEvaluator{answers: map[jev.Hazard]float64{jev.HazardDroppedError: 0.96}}
	usage := agents.NewUsageCollector()
	result, err := reviewAnalyst{evaluator: evaluator, usage: usage}.ReviewPatch(context.Background(), orchestrator.ReviewRequest{
		Campaign: orchestrator.CampaignRequest{Repository: repo},
		Proposal: agents.OptimizerResult{Hypothesis: "buffer the writes", Patch: writePatch},
	})
	require.NoError(t, err)
	require.False(t, result.Proceed)
	require.Len(t, result.Concerns, 1)
	require.Contains(t, result.Concerns[0], "an error from a call that can fail is discarded (Jev yes 0.96")
	require.Len(t, result.RequiredChecks, 1)
	require.Equal(t, "write", evaluator.state["function"])
	require.Contains(t, evaluator.state["source"], "// write prints every record")
	require.Equal(t, writePatch, evaluator.state["patch"])
	require.Equal(t, int64(1), usage.Snapshot()[string(agents.RoleReviewer)].Requests)
}

func TestReviewWithNothingRaisedProceeds(t *testing.T) {
	result, err := reviewAnalyst{evaluator: &hazardEvaluator{}}.ReviewPatch(context.Background(), orchestrator.ReviewRequest{
		Proposal: agents.OptimizerResult{Patch: writePatch},
	})
	require.NoError(t, err)
	require.True(t, result.Proceed)
	require.Empty(t, result.Concerns)
}

func TestReviewOfAnEmptyPatchAsksNothing(t *testing.T) {
	evaluator := &hazardEvaluator{}
	result, err := reviewAnalyst{evaluator: evaluator}.ReviewPatch(context.Background(), orchestrator.ReviewRequest{})
	require.NoError(t, err)
	require.Equal(t, "no patch to review", result.BehaviorArgument)
	require.Nil(t, evaluator.state)
}

func TestReviewPassesOnGatewayAndRankingFailures(t *testing.T) {
	_, err := reviewAnalyst{evaluator: &hazardEvaluator{err: errors.New("HTTP 429")}}.ReviewPatch(context.Background(), orchestrator.ReviewRequest{Proposal: agents.OptimizerResult{Patch: writePatch}})
	require.ErrorContains(t, err, "HTTP 429")
	_, err = reviewAnalyst{evaluator: &hazardEvaluator{answers: map[jev.Hazard]float64{jev.HazardConcurrency: 2}}}.ReviewPatch(context.Background(), orchestrator.ReviewRequest{Proposal: agents.OptimizerResult{Patch: writePatch}})
	require.ErrorContains(t, err, "outside [0, 1]")
}

func TestFirstChangeFindsTheOldSideLine(t *testing.T) {
	path, line, ok := firstChange(writePatch)
	require.True(t, ok)
	require.Equal(t, "fixture.go", path)
	require.Equal(t, 13, line)
	_, _, ok = firstChange("not a diff")
	require.False(t, ok)
	require.Equal(t, hotFunction{}, patchedFunction(t.TempDir(), writePatch))
}

func TestReviewConcernsAreRecordedAndReported(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	roles := agents.Set{ReviewEvaluator: jev.Stub{}}
	engine.SetADK(&roles, nil)
	_, err := adkServices{engine: engine}.Evaluate(context.Background(), orchestrator.PolicyInput{
		Evidence: orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "candidate-1"}},
		Review:   agents.ReviewerResult{Concerns: []string{"an error from a call that can fail is discarded (Jev yes 0.96, +5.0 sd)"}},
	})
	require.NoError(t, err)
	require.Equal(t, ReviewerJev, engine.State().Reviewer)
	report := RenderMarkdown(engine.State())
	require.Contains(t, report, "- Reviewer: Jev behaviour-hazard checks (`typesafe-ai/jev`)")
	require.Contains(t, report, "- Review: an error from a call that can fail is discarded (Jev yes 0.96, +5.0 sd)")
}
