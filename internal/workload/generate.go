// Package workload validates declarative proposals and reads the boolean
// options a target's source declares. Proposal commands are argument
// vectors, never shell expressions.
package workload

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
)

func ValidateProposal(proposal agents.WorkloadProposal, m manifest.Manifest) error {
	if err := validateProposalMeta(proposal); err != nil {
		return err
	}
	if err := validateProposalArgs(proposal.Arguments); err != nil {
		return err
	}
	if err := validateProposalFixtures(proposal.Fixtures); err != nil {
		return err
	}
	return validateScalingDimensions(proposal.ScalingDimensions, m)
}

func validateProposalMeta(proposal agents.WorkloadProposal) error {
	if strings.TrimSpace(proposal.Name) == "" {
		return errors.New("proposal name is required")
	}
	if strings.TrimSpace(proposal.Provenance) == "" {
		return errors.New("proposal provenance is required")
	}
	switch proposal.Tier {
	case domain.TierRepresentative, domain.TierPlausible, domain.TierStress:
		return nil
	default:
		return fmt.Errorf("invalid workload tier %q", proposal.Tier)
	}
}

func validateProposalArgs(args []string) error {
	if len(args) > 256 {
		return errors.New("too many arguments")
	}
	for _, argument := range args {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("argument contains NUL")
		}
	}
	return nil
}

func validateProposalFixtures(fixtures []agents.ProposedFixture) error {
	for _, fixture := range fixtures {
		if fixtureEscapes(fixture.Path) {
			return fmt.Errorf("fixture path %q escapes sandbox", fixture.Path)
		}
	}
	return nil
}

func fixtureEscapes(path string) bool {
	clean := filepath.Clean(path)
	return path == "" || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validateScalingDimensions(dims map[string]int, m manifest.Manifest) error {
	limit := m.Workloads.Discovery.MaxCases * max(1, m.Workloads.Discovery.MaxDepth)
	for name, value := range dims {
		if name == "" || value < 0 || value > limit {
			return fmt.Errorf("invalid scaling dimension %q=%d", name, value)
		}
	}
	return nil
}
