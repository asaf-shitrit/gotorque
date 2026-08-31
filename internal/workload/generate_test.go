package workload

import (
	"path/filepath"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"github.com/stretchr/testify/require"
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

func validManifest() manifest.Manifest {
	return manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 10, MaxDepth: 3}}}
}

func TestValidateProposalMeta(t *testing.T) {
	cases := []struct {
		name     string
		proposal agents.WorkloadProposal
		wantErr  bool
	}{
		{
			name:     "empty name",
			proposal: agents.WorkloadProposal{Name: "  ", Provenance: "test", Tier: domain.TierPlausible},
			wantErr:  true,
		},
		{
			name:     "empty provenance",
			proposal: agents.WorkloadProposal{Name: "ok", Provenance: "  ", Tier: domain.TierPlausible},
			wantErr:  true,
		},
		{
			name:     "invalid tier",
			proposal: agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.WorkloadTier("bogus")},
			wantErr:  true,
		},
		{
			name:     "representative tier valid",
			proposal: agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierRepresentative},
			wantErr:  false,
		},
		{
			name:     "plausible tier valid",
			proposal: agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierPlausible},
			wantErr:  false,
		},
		{
			name:     "stress tier valid",
			proposal: agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierStress},
			wantErr:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProposal(tc.proposal, validManifest())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateProposalArgs(t *testing.T) {
	base := agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierPlausible}

	t.Run("too many arguments", func(t *testing.T) {
		p := base
		p.Arguments = make([]string, 257)
		require.Error(t, ValidateProposal(p, validManifest()))
	})

	t.Run("NUL byte in argument", func(t *testing.T) {
		p := base
		p.Arguments = []string{"ok", "bad\x00arg"}
		require.Error(t, ValidateProposal(p, validManifest()))
	})

	t.Run("256 arguments allowed", func(t *testing.T) {
		p := base
		p.Arguments = make([]string, 256)
		require.NoError(t, ValidateProposal(p, validManifest()))
	})

	t.Run("clean arguments allowed", func(t *testing.T) {
		p := base
		p.Arguments = []string{"-x", "value"}
		require.NoError(t, ValidateProposal(p, validManifest()))
	})
}

func TestValidateProposalFixtures(t *testing.T) {
	base := agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierPlausible}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty path", "", true},
		{"absolute path", "/etc/passwd", true},
		{"exact dot-dot", "..", true},
		{"parent traversal", "../secret", true},
		{"nested traversal", "a/../../b", true},
		{"clean relative path", "fixtures/input.json", false},
		{"dot path", "./fixtures/input.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Fixtures = []agents.ProposedFixture{{Path: tc.path}}
			err := ValidateProposal(p, validManifest())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateScalingDimensions(t *testing.T) {
	m := manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 10, MaxDepth: 3}}}
	base := agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierPlausible}

	cases := []struct {
		name    string
		dims    map[string]int
		wantErr bool
	}{
		{"empty name key", map[string]int{"": 1}, true},
		{"negative value", map[string]int{"depth": -1}, true},
		{"value exceeds limit", map[string]int{"depth": 31}, true},
		{"value at limit", map[string]int{"depth": 30}, false},
		{"zero value allowed", map[string]int{"depth": 0}, false},
		{"nil dims allowed", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.ScalingDimensions = tc.dims
			err := ValidateProposal(p, m)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateScalingDimensionsMaxDepthZeroFallsBackToOne(t *testing.T) {
	// max(1, m.Workloads.Discovery.MaxDepth) means a manifest with MaxDepth=0
	// still permits a scaling dimension up to MaxCases.
	m := manifest.Manifest{Workloads: manifest.WorkloadConfiguration{Discovery: manifest.DiscoverySettings{MaxCases: 5, MaxDepth: 0}}}
	p := agents.WorkloadProposal{Name: "ok", Provenance: "test", Tier: domain.TierPlausible, ScalingDimensions: map[string]int{"depth": 5}}
	require.NoError(t, ValidateProposal(p, m))
	p.ScalingDimensions["depth"] = 6
	require.Error(t, ValidateProposal(p, m))
}

func TestJSONShapeProposals(t *testing.T) {
	proposals := JSONShapeProposals(42, Ordinary, 3)
	require.Len(t, proposals, 3)
	seen := map[string]bool{}
	for _, p := range proposals {
		require.Equal(t, domain.TierPlausible, p.Tier)
		require.Equal(t, "deterministic-json-shape", p.Provenance)
		require.True(t, p.ExpectedValid)
		require.Equal(t, []string{"."}, p.Arguments)
		require.NotEmpty(t, p.Stdin)
		require.False(t, seen[p.Name], "duplicate proposal name %q", p.Name)
		seen[p.Name] = true
	}

	// Deterministic: same base/namespace produce the same proposals.
	again := JSONShapeProposals(42, Ordinary, 3)
	require.Equal(t, proposals, again)

	// A different namespace must diverge (independent seed stream).
	holdout := JSONShapeProposals(42, HiddenHoldout, 3)
	require.NotEqual(t, proposals, holdout)
}

func TestJSONShapeProposalsClampsMaxCases(t *testing.T) {
	proposals := JSONShapeProposals(1, Ordinary, 1000)
	require.Len(t, proposals, 5) // bounded by the number of known shapes
}

func TestFileTreeProposals(t *testing.T) {
	proposals := FileTreeProposals(7, Ordinary, 3, 4)
	require.Len(t, proposals, 3)
	// Result is sorted by name.
	for i := 1; i < len(proposals); i++ {
		require.LessOrEqual(t, proposals[i-1].Name, proposals[i].Name)
	}
	for _, p := range proposals {
		require.Equal(t, domain.TierPlausible, p.Tier)
		require.Equal(t, "deterministic-file-tree", p.Provenance)
		require.True(t, p.ExpectedValid)
		require.Len(t, p.Fixtures, 1)
		fixture := p.Fixtures[0]
		require.False(t, filepath.IsAbs(fixture.Path))
		require.NotContains(t, fixture.Path, "..")
		depth, ok := p.ScalingDimensions["depth"]
		require.True(t, ok)
		require.GreaterOrEqual(t, depth, 1)
		require.LessOrEqual(t, depth, 4)
	}

	again := FileTreeProposals(7, Ordinary, 3, 4)
	require.Equal(t, proposals, again)
}

func TestFileTreeProposalsClampsMaxCases(t *testing.T) {
	proposals := FileTreeProposals(1, Ordinary, 1000, 2)
	require.Len(t, proposals, 4) // bounded by the number of known extensions
}

func TestFileTreeProposalsMaxDepthZeroFallsBackToOne(t *testing.T) {
	proposals := FileTreeProposals(1, Ordinary, 1, 0)
	require.Len(t, proposals, 1)
	require.Equal(t, 1, proposals[0].ScalingDimensions["depth"])
}
