package campaign

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/jev"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
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
	return agents.Set{Optimizer: unreachableAgent(t, "optimizer"), Jev: jev.Stub{}}
}

// TestProviderOutageFailsTheCampaignAndResumes: a campaign whose optimizer
// failed every cycle used to end `completed` at the rejection bound, blaming
// its empty patches, and a completed campaign cannot be resumed. It now fails
// after two consecutive failed cycles, names the provider, and resumes once
// the optimizer answers again.
func TestProviderOutageFailsTheCampaignAndResumes(t *testing.T) {
	roles := unreachableRoles(t)
	cfg := orchestrator.Config{MaxCandidates: 4, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Minute, AgentTimeout: time.Minute}
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{
		Repository:                    makeRepository(t),
		ManifestPath:                  writeManifest(t, t.TempDir()),
		CampaignDir:                   campaignDir,
		TestingUnsafeDisableIsolation: true,
		FreeChoice:                    true,
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
	require.Len(t, state.CandidateRecords, 2, "two consecutive failed cycles must be enough to stop")
	for _, record := range state.CandidateRecords {
		require.Equal(t, domain.DecisionRejected, record.Decision, "the breaker must not change the verdict")
	}
	snapshot, err := loadReportSnapshot(filepath.Join(campaignDir, ReportJSONName))
	require.NoError(t, err)
	require.Equal(t, StatusFailed, snapshot.Status, "the report on disk must carry the terminal status")
	require.True(t, strings.HasPrefix(snapshot.ProviderFailure, "optimizer: "), "the report must name the failed role apart from the prose: %q", snapshot.ProviderFailure)
	require.Contains(t, RenderMarkdown(snapshot), "- Failed role: optimizer: ")
	require.NoError(t, engine.Close())

	resumed, err := Resume(campaignDir, nil)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	working := rejectingRoles(t)
	resumed.SetADK(&working, &cfg)
	require.NoError(t, resumed.Run(context.Background()))
	require.Equal(t, StatusCompleted, resumed.State().Status)
	require.Empty(t, resumed.State().ProviderFailure, "a resume that completes clears the failure")
	// The two failed cycles count against the budget of four, so the resumed
	// process spends the remaining two (#91).
	require.Equal(t, "maximum candidate count reached", resumed.State().StopReason)
	require.Len(t, resumed.State().CandidateRecords, 4)
}

// TestNotesBecomeTheEventsReadersKnow: the graph's lifecycle notes are saved
// under the event kinds the report, scorecard and triage read. A finished note
// carries the result as given: whether the campaign failed is decided where it
// ended (CampaignResult.ProviderFailure), not derived again here.
func TestNotesBecomeTheEventsReadersKnow(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	notes := adkServices{engine: engine}
	ctx := context.Background()
	require.NoError(t, notes.Note(ctx, orchestrator.Note{Kind: orchestrator.NoteStarted, Request: orchestrator.CampaignRequest{CampaignID: "c1"}}))
	require.NoError(t, notes.Note(ctx, orchestrator.Note{Kind: orchestrator.NoteDegraded, Role: "analyst", Cause: "HTTP 429"}))
	require.NoError(t, notes.Note(ctx, orchestrator.Note{Kind: orchestrator.NoteRepaired, Role: "optimizer", Repair: agents.RepairTerminatedString}))
	require.NoError(t, notes.Note(ctx, orchestrator.Note{Kind: orchestrator.NoteFinished, Result: orchestrator.CampaignResult{ProviderFailure: "reviewer: 401 Unauthorized", StopReason: "model provider unavailable"}}))

	require.Equal(t, []string{"adk_started", "role_degraded", "role_repaired", "adk_finalized"}, eventKinds(t, engine))
	require.Equal(t, []RoleDegradation{{Role: "analyst", Cause: "HTTP 429"}}, engine.state.DegradedRoles)
	events, err := engine.store.Events()
	require.NoError(t, err)
	require.Equal(t, "model provider unavailable", events[3].Message)
	require.Contains(t, events[1].Message, "analyst node failed, continuing with an empty result: HTTP 429")
	require.Contains(t, events[2].Message, "optimizer output parsed only after the decoder "+string(agents.RepairTerminatedString))

	require.Error(t, notes.Note(ctx, orchestrator.Note{Kind: "bogus"}))
}

// A salvaged patch is judged exactly like any other; the repair only rides to
// the record and the report so it does not read as the patch the model sent.
func TestSettleCarriesTheProposalRepairWithoutJudgingIt(t *testing.T) {
	evidence := orchestrator.CandidateEvidence{
		Candidate:          domain.Candidate{ID: "candidate-1", Hypothesis: "buffer output"},
		BehaviorMatches:    true,
		SafetyChecksPassed: true,
		Summary:            "patch applied",
	}
	plain := settleJudged(t, pgoLaneTestEngine(t), evidence, nil, agents.ReviewerResult{})

	engine := pgoLaneTestEngine(t)
	evidence.ProposalRepair = agents.RepairTerminatedString
	repaired := settleJudged(t, engine, evidence, nil, agents.ReviewerResult{})

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

// A build failure used to reach the record and the report as "exit status 1"
// alone: the compiler's stderr was captured on the evidence and then dropped.
func TestSettleRecordsTheBuildFailureDetail(t *testing.T) {
	engine := pgoLaneTestEngine(t)
	evidence := orchestrator.CandidateEvidence{
		Candidate:     domain.Candidate{ID: "candidate-1", Hypothesis: "concatenate"},
		Summary:       "candidate build failed: exit status 1",
		FailureDetail: "execution/context.go:5:2: \"fmt\" imported and not used",
		Unmeasured:    true,
	}
	settleJudged(t, engine, evidence, nil, agents.ReviewerResult{})
	require.Len(t, engine.state.CandidateRecords, 1)
	require.Equal(t, evidence.FailureDetail, engine.state.CandidateRecords[0].FailureDetail)

	report := RenderMarkdown(State{CandidateRecords: engine.state.CandidateRecords})
	require.Contains(t, report, "- Failure detail:\n\n```text\nexecution/context.go:5:2: \"fmt\" imported and not used\n```")
}

func TestRenderMarkdownOmitsAnEmptyFailureDetail(t *testing.T) {
	record := CandidateRecord{Attempt: 1, CandidateID: "cand-1", Decision: domain.DecisionInconclusive}
	require.NotContains(t, RenderMarkdown(State{CandidateRecords: []CandidateRecord{record}}), "Failure detail")
}
