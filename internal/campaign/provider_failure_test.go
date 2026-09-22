package campaign

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// unreachableAgent fails every call, the way every role does once the provider
// is down or the key has been revoked.
func unreachableAgent(t *testing.T, name string) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{Name: name, Run: func(adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			yield(nil, errors.New("POST /chat/completions: 401 Unauthorized"))
		}
	}})
	require.NoError(t, err)
	return a
}

func unreachableRoles(t *testing.T) agents.Set {
	t.Helper()
	return agents.Set{
		Coordinator: unreachableAgent(t, "coordinator"),
		Explorer:    unreachableAgent(t, "explorer"),
		Analyst:     unreachableAgent(t, "analyst"),
		Optimizer:   unreachableAgent(t, "optimizer"),
		Reviewer:    unreachableAgent(t, "reviewer"),
	}
}

// TestProviderOutageFailsTheCampaignAndResumes: a campaign whose every model
// role failed used to end `completed` at the rejection bound, blaming its empty
// patches, and a completed campaign cannot be resumed. It now fails after one
// cycle, names the provider, and resumes once the roles answer again.
func TestProviderOutageFailsTheCampaignAndResumes(t *testing.T) {
	roles := unreachableRoles(t)
	cfg := orchestrator.Config{MaxCandidates: 4, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Minute, AgentTimeout: time.Minute}
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{
		Repository:                    makeRepository(t),
		ManifestPath:                  writeManifest(t, t.TempDir()),
		CampaignDir:                   campaignDir,
		TestingUnsafeDisableIsolation: true,
		ADKAgents:                     &roles,
		ADKConfig:                     &cfg,
	})
	require.NoError(t, err)

	err = engine.Run(context.Background())
	require.ErrorIs(t, err, orchestrator.ErrProviderUnavailable)
	state := engine.State()
	require.Equal(t, StatusFailed, state.Status)
	require.True(t, strings.HasPrefix(state.StopReason, "model provider unavailable"), "stop reason %q must name the provider", state.StopReason)
	require.Contains(t, state.StopReason, "401 Unauthorized")
	require.False(t, state.CompletedSteps["complete"], "a campaign stopped by its provider must not be reported as finished")
	require.Len(t, state.CandidateRecords, 1, "one cycle of total failure must be enough to stop")
	require.Equal(t, domain.DecisionRejected, state.CandidateRecords[0].Decision, "the breaker must not change the verdict")
	snapshot, err := loadReportSnapshot(filepath.Join(campaignDir, ReportJSONName))
	require.NoError(t, err)
	require.Equal(t, StatusFailed, snapshot.Status, "the report on disk must carry the terminal status")
	require.NoError(t, engine.Close())

	resumed, err := Resume(campaignDir, nil)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	working := rejectingRoles(t)
	resumed.SetADK(&working, &cfg)
	require.NoError(t, resumed.Run(context.Background()))
	require.Equal(t, StatusCompleted, resumed.State().Status)
	require.Equal(t, "consecutive rejection/inconclusive limit reached", resumed.State().StopReason)
}

func TestCompleteCampaignFailsTheJobOnlyForAProviderFailure(t *testing.T) {
	services := adkServices{engine: pgoLaneTestEngine(t)}
	job, err := services.CompleteCampaign(context.Background(), domain.Job{ID: "job-1"}, orchestrator.CampaignResult{StopReason: "maximum candidate count reached"})
	require.NoError(t, err)
	require.Equal(t, domain.JobSucceeded, job.Status)

	job, err = services.CompleteCampaign(context.Background(), domain.Job{ID: "job-1"}, orchestrator.CampaignResult{ProviderFailure: "reviewer: 401 Unauthorized"})
	require.NoError(t, err)
	require.Equal(t, domain.JobFailed, job.Status)
}

func TestRecordRoleRepairedPersistsAnEvent(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	require.NoError(t, adkServices{engine: engine}.RecordRoleRepaired(context.Background(), "optimizer", agents.RepairTerminatedString))

	events, err := engine.store.Events()
	require.NoError(t, err)
	var found bool
	for _, event := range events {
		if event.Type == "role_repaired" {
			found = true
			require.Contains(t, event.Message, "optimizer")
			require.Contains(t, event.Message, string(agents.RepairTerminatedString))
		}
	}
	require.True(t, found, "the repair must be persisted as an event")
}

// A salvaged patch is judged exactly like any other; the repair only rides to
// the record and the report so it does not read as the patch the model sent.
func TestEvaluateCarriesTheProposalRepairWithoutJudgingIt(t *testing.T) {
	evidence := orchestrator.CandidateEvidence{
		Candidate:          domain.Candidate{ID: "candidate-1", Hypothesis: "buffer output"},
		BehaviorMatches:    true,
		SafetyChecksPassed: true,
		Summary:            "patch applied",
	}
	plain, err := adkServices{engine: pgoLaneTestEngine(t)}.Evaluate(context.Background(), orchestrator.PolicyInput{Evidence: evidence})
	require.NoError(t, err)

	engine := pgoLaneTestEngine(t)
	evidence.ProposalRepair = agents.RepairTerminatedString
	repaired, err := adkServices{engine: engine}.Evaluate(context.Background(), orchestrator.PolicyInput{Evidence: evidence})
	require.NoError(t, err)

	require.Equal(t, plain.Decision, repaired.Decision)
	require.Equal(t, plain.Reasons, repaired.Reasons)
	require.Len(t, engine.state.CandidateRecords, 1)
	require.Equal(t, string(agents.RepairTerminatedString), engine.state.CandidateRecords[0].ProposalRepair)
}

func TestRenderMarkdownNamesASalvagedProposal(t *testing.T) {
	record := CandidateRecord{Attempt: 1, CandidateID: "cand-1", Decision: domain.DecisionRejected, ProposalRepair: string(agents.RepairTerminatedString)}
	report := RenderMarkdown(State{CandidateRecords: []CandidateRecord{record}})
	require.Contains(t, report, "- Proposal salvaged: the optimizer's output parsed only after the decoder "+string(agents.RepairTerminatedString))

	record.ProposalRepair = ""
	require.NotContains(t, RenderMarkdown(State{CandidateRecords: []CandidateRecord{record}}), "Proposal salvaged", "a proposal that parsed as sent must not carry the line")
}
