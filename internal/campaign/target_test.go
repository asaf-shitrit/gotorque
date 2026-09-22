package campaign

import (
	"context"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

var bufioTarget = agents.Target{Location: "main.go:206", Function: "gron", Cause: "unbuffered_io", Remedy: "Route the writes in gron through bufio.", Z: 3.3}

// TestVerdictsRecordTheirTargetAndResumeReadsThemBack: the target a candidate
// was told to attack is persisted with its verdict, named in the report, and
// handed back to the graph on resume so it is not attacked twice.
func TestVerdictsRecordTheirTargetAndResumeReadsThemBack(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	services := adkServices{engine: engine}
	_, err := services.Evaluate(context.Background(), orchestrator.PolicyInput{
		Evidence: orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "candidate-1", Hypothesis: "buffer gron's output"}},
		Target:   &bufioTarget,
	})
	require.NoError(t, err)
	_, err = services.Evaluate(context.Background(), orchestrator.PolicyInput{
		Evidence: orchestrator.CandidateEvidence{Candidate: domain.Candidate{ID: "candidate-2"}},
	})
	require.NoError(t, err)

	require.Equal(t, &bufioTarget, engine.state.CandidateRecords[0].Target)
	require.Nil(t, engine.state.CandidateRecords[1].Target)
	require.Equal(t, []agents.Target{bufioTarget}, engine.priorTargets())
	require.Contains(t, RenderMarkdown(engine.state), "- Target: `gron` at `main.go:206`, unbuffered_io (+3.3 sd)")
}
