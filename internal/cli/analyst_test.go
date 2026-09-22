package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/jev"
	"github.com/stretchr/testify/require"
)

func TestAnalystFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags optimizeFlags
		want  string
	}{
		{name: "default", flags: optimizeFlags{}},
		{name: "llm", flags: optimizeFlags{analyst: analystLLM}},
		{name: "jev with adk", flags: optimizeFlags{analyst: analystJev, runADK: true}},
		{name: "jev with stub", flags: optimizeFlags{analyst: analystJev, runADKStub: true}},
		{name: "jev alone", flags: optimizeFlags{analyst: analystJev}, want: "needs --adk or --adk-stub"},
		{name: "unknown", flags: optimizeFlags{analyst: "gpt"}, want: `unknown --analyst "gpt"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJevRoles(tc.flags)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestOptimizeRejectsJevWithoutTheAgentGraph(t *testing.T) {
	cmd := New(Dependencies{Stdout: io.Discard, Stderr: io.Discard})
	cmd.SetArgs([]string{"optimize", "--repo", t.TempDir(), "--manifest", "m.json", "--analyst", "jev"})
	require.ErrorContains(t, cmd.Execute(), "--analyst jev needs --adk or --adk-stub")
}

func TestSelectAnalystUsesTheStubUnderADKStub(t *testing.T) {
	roles, config, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{runADKStub: true, analyst: analystJev})
	require.NoError(t, err)
	require.NotNil(t, config)
	require.Equal(t, jev.Stub{}, roles.CauseEvaluator)
}

func TestSelectAnalystLeavesTheModelRoleByDefault(t *testing.T) {
	roles, _, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{runADKStub: true})
	require.NoError(t, err)
	require.Nil(t, roles.CauseEvaluator)
}

func TestSelectAnalystPreflightsTheGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"ok":{"type":"boolean","probability":1}}}`))
	}))
	defer srv.Close()
	t.Setenv(jev.EnvAPIKey, "key")
	t.Setenv(jev.EnvBaseURL, srv.URL)
	var out bytes.Buffer
	roles := &agents.Set{}
	require.NoError(t, selectJev(context.Background(), &out, roles, optimizeFlags{runADK: true, analyst: analystJev}))
	require.IsType(t, jev.Client{}, roles.CauseEvaluator)
	require.Contains(t, out.String(), "analyst: Jev cause classification (typesafe-ai/jev)")
}

func TestSelectAnalystStopsOnAFailedPreflight(t *testing.T) {
	t.Setenv(jev.EnvAPIKey, "")
	roles := &agents.Set{}
	err := selectJev(context.Background(), io.Discard, roles, optimizeFlags{runADK: true, analyst: analystJev})
	require.ErrorContains(t, err, jev.EnvAPIKey)
	require.Nil(t, roles.CauseEvaluator)
}

func TestAttachResumeADKRequiresTheAnalystAJevCampaignStartedWith(t *testing.T) {
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live", Analyst: campaign.AnalystJev})
	err := attachResumeADK(context.Background(), io.Discard, engine, optimizeFlags{resume: "campaign-dir", runADKStub: true}, nil, nil)
	require.ErrorContains(t, err, "started with --analyst jev")

	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, optimizeFlags{resume: "campaign-dir", runADKStub: true, analyst: analystJev}, nil, nil))
	require.Equal(t, campaign.AnalystJev, engine.State().Analyst)
}

func TestReviewerFlagFollowsTheAnalystRules(t *testing.T) {
	require.NoError(t, validateJevRoles(optimizeFlags{reviewer: analystJev, runADKStub: true}))
	require.ErrorContains(t, validateJevRoles(optimizeFlags{reviewer: analystJev}), "--reviewer jev needs --adk or --adk-stub")
	require.ErrorContains(t, validateJevRoles(optimizeFlags{reviewer: "gpt"}), `unknown --reviewer "gpt"`)
}

func TestSelectJevCanReplaceBothRolesWithOneClient(t *testing.T) {
	roles := &agents.Set{}
	var out bytes.Buffer
	require.NoError(t, selectJev(context.Background(), &out, roles, optimizeFlags{runADKStub: true, analyst: analystJev, reviewer: analystJev}))
	require.Equal(t, jev.Stub{}, roles.CauseEvaluator)
	require.Equal(t, jev.Stub{}, roles.ReviewEvaluator)
	require.Contains(t, out.String(), "reviewer: Jev behaviour-hazard checks")

	reviewerOnly := &agents.Set{}
	require.NoError(t, selectJev(context.Background(), io.Discard, reviewerOnly, optimizeFlags{runADKStub: true, reviewer: analystJev}))
	require.Nil(t, reviewerOnly.CauseEvaluator)
	require.NotNil(t, reviewerOnly.ReviewEvaluator)
}

func TestAttachResumeADKRequiresTheReviewerAJevCampaignStartedWith(t *testing.T) {
	engine := resumeEngine(t, campaign.State{ID: "campaign-test", ADKMode: "live", Reviewer: campaign.ReviewerJev})
	err := attachResumeADK(context.Background(), io.Discard, engine, optimizeFlags{resume: "campaign-dir", runADKStub: true}, nil, nil)
	require.ErrorContains(t, err, "started with --reviewer jev")
	require.NoError(t, attachResumeADK(context.Background(), io.Discard, engine, optimizeFlags{resume: "campaign-dir", runADKStub: true, reviewer: analystJev}, nil, nil))
}
