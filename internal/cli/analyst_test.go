package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/campaign"
	"github.com/asaf-shitrit/gotorque/internal/jev"
	"github.com/stretchr/testify/require"
)

func TestAttachJevUsesTheStubUnderADKStub(t *testing.T) {
	roles, config, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{runADKStub: true})
	require.NoError(t, err)
	require.NotNil(t, config)
	require.Equal(t, jev.Stub{}, roles.Jev)
}

func TestAttachJevPreflightsTheGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"ok":{"type":"boolean","probability":1}}}`))
	}))
	defer srv.Close()
	t.Setenv(jev.EnvAPIKey, "key")
	t.Setenv(jev.EnvBaseURL, srv.URL)
	// This stub answers every question with the same fixed probability, so
	// Preflight's canary and release-date checks see drift against jev's
	// real recorded values; the override downgrades that to a warning, which
	// is all this test cares about proving (the gateway is reached and the
	// roles get wired up), not the drift guards themselves (covered in
	// internal/jev).
	t.Setenv(jev.EnvAllowDrift, "1")
	var out bytes.Buffer
	roles := &agents.Set{}
	require.NoError(t, attachJev(context.Background(), &out, roles, optimizeFlags{runADK: true}))
	require.IsType(t, jev.Client{}, roles.Jev)
	require.Contains(t, out.String(), "analyst, reviewer and explorer: Jev (typesafe/jev-1.13-20260917)")
}

func TestAttachJevStopsOnAFailedPreflight(t *testing.T) {
	t.Setenv(jev.EnvAPIKey, "")
	roles := &agents.Set{}
	err := attachJev(context.Background(), io.Discard, roles, optimizeFlags{runADK: true})
	require.ErrorContains(t, err, jev.EnvAPIKey)
	require.Nil(t, roles.Jev)
}

// A campaign that ran model roles cannot resume on this build, which has only
// Jev for them; a Jev campaign resumes, and so does one that never ran the
// graph (ADKMode empty).
func TestRequireJevCampaign(t *testing.T) {
	f := optimizeFlags{resume: "campaign-dir"}
	err := requireJevCampaign(campaign.State{ADKMode: "live", Analyst: ""}, f)
	require.ErrorContains(t, err, "ran model roles this build no longer has")
	require.ErrorContains(t, err, "campaign-dir")

	require.NoError(t, requireJevCampaign(campaign.State{ADKMode: "live", Analyst: campaign.AnalystJev}, f))
	require.NoError(t, requireJevCampaign(campaign.State{}, f))
}
