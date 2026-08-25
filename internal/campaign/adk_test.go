package campaign

import (
	"context"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"

	"github.com/stretchr/testify/require"
)

func TestDiscoverValidatesExplorerProposals(t *testing.T) {
	engine := &Engine{state: State{Manifest: manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 10, MaxDepth: 3}}}}}
	valid := agents.WorkloadProposal{Name: "smoke", Arguments: []string{"."}, Tier: domain.TierPlausible, Provenance: "test", ExpectedValid: true}
	invalid := agents.WorkloadProposal{Name: "escape", Tier: domain.TierPlausible, Provenance: "test", Fixtures: []agents.ProposedFixture{{Path: "../escape"}}}

	evidence, err := adkServices{engine: engine}.Discover(context.Background(), orchestrator.DiscoveryRequest{
		Explorer: agents.ExplorerResult{Proposals: []agents.WorkloadProposal{valid, invalid}},
	})
	require.NoError(t, err)
	require.Equal(t, "1", evidence.Metadata["proposals_accepted"])
	require.Equal(t, "1", evidence.Metadata["proposals_rejected"])
	require.Contains(t, evidence.Metadata["proposal_rejections"], "escape")
	require.Contains(t, evidence.Metadata["proposal_rejections"], "escapes sandbox")
	require.Contains(t, evidence.Summary, "(1/2 explorer proposals valid)")
}

func TestDiscoverAcceptsAllValidProposalsWithoutRejectionMetadata(t *testing.T) {
	engine := &Engine{state: State{}}
	evidence, err := adkServices{engine: engine}.Discover(context.Background(), orchestrator.DiscoveryRequest{})
	require.NoError(t, err)
	require.Equal(t, "0", evidence.Metadata["proposals_accepted"])
	require.Equal(t, "0", evidence.Metadata["proposals_rejected"])
	require.NotContains(t, evidence.Metadata, "proposal_rejections")
}
