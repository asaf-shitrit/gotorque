package workload

import (
	"example.com/go-agent-optimizer/internal/agents"
	"example.com/go-agent-optimizer/internal/domain"
	"example.com/go-agent-optimizer/internal/manifest"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSeedNamespacesAreDeterministicAndSeparate(t *testing.T) {
	require.Equal(t, Seed(42, Ordinary), Seed(42, Ordinary))
	require.NotEqual(t, Seed(42, Ordinary), Seed(42, HiddenHoldout))
	require.Equal(t, SizeSweep(42, Ordinary, []int{1, 2, 3}), SizeSweep(42, Ordinary, []int{1, 2, 3}))
}

func TestValidateProposalRejectsShellIndependentEscape(t *testing.T) {
	m := manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 10, MaxDepth: 3}}}
	err := ValidateProposal(agents.WorkloadProposal{Name: "bad", Tier: domain.TierPlausible, Provenance: "test", Fixtures: []agents.ProposedFixture{{Path: "../escape"}}}, m)
	require.Error(t, err)
}
