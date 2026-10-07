package campaign

import (
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

var bufioTarget = agents.Target{Location: "main.go:206", Function: "gron", Cause: "unbuffered_io", Remedy: "Route the writes in gron through bufio.", Z: 3.3}

// TestVerdictsRecordTheirTargetAndResumeReadsThemBack: the target a candidate
// was told to attack is persisted with its verdict, named in the report, and
// handed back to the graph on resume so it is not attacked twice.
func TestVerdictsRecordTheirTargetAndResumeReadsThemBack(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	settleJudged(t, engine, orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "candidate-1", Hypothesis: "buffer gron's output"}}, &bufioTarget, agents.ReviewerResult{})
	settleJudged(t, engine, orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "candidate-2"}}, nil, agents.ReviewerResult{})

	require.Equal(t, &bufioTarget, engine.state.CandidateRecords[0].Target)
	require.Nil(t, engine.state.CandidateRecords[1].Target)
	recorded := engine.campaignRequest().RecordedCandidates
	require.Len(t, recorded, 2)
	require.Equal(t, &bufioTarget, recorded[0].Target)
	require.Nil(t, recorded[1].Target)
	require.Contains(t, RenderMarkdown(engine.state), "- Target: `gron` at `main.go:206`, unbuffered_io (+3.3 sd)")
}
