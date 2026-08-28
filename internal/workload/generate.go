// Package workload validates declarative proposals and provides deterministic
// target-aware case generation. Generated commands are argument vectors, never
// shell expressions.
package workload

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
)

type Namespace string

const (
	Ordinary      Namespace = "ordinary"
	HiddenHoldout Namespace = "hidden-holdout"
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

// Seed derives independent reproducible random streams so ordinary generated
// cases cannot reveal hidden holdout inputs.
func Seed(base uint64, namespace Namespace) [2]uint64 {
	var input [8]byte
	binary.LittleEndian.PutUint64(input[:], base)
	sum := sha256.Sum256(append(input[:], []byte(namespace)...))
	return [2]uint64{binary.LittleEndian.Uint64(sum[:8]), binary.LittleEndian.Uint64(sum[8:16])}
}

func SizeSweep(base uint64, namespace Namespace, sizes []int) []int {
	values := append([]int(nil), sizes...)
	seed := Seed(base, namespace)
	r := rand.New(rand.NewPCG(seed[0], seed[1]))
	r.Shuffle(len(values), func(i, j int) { values[i], values[j] = values[j], values[i] })
	return values
}

// JSONShapeProposals creates bounded gojq-style input shapes.
func JSONShapeProposals(base uint64, namespace Namespace, maxCases int) []agents.WorkloadProposal {
	shapes := []string{"null\n", "[]\n", "{}\n", "[0,1,-1,128]\n", "{\"items\":[{\"value\":0},{\"value\":1}]}\n"}
	order := SizeSweep(base, namespace, []int{0, 1, 2, 3, 4})
	if maxCases > len(order) {
		maxCases = len(order)
	}
	result := make([]agents.WorkloadProposal, 0, maxCases)
	for _, index := range order[:maxCases] {
		result = append(result, agents.WorkloadProposal{Name: fmt.Sprintf("json-shape-%d", index), Arguments: []string{"."}, Stdin: shapes[index], Tier: domain.TierPlausible, Provenance: "deterministic-json-shape", ExpectedValid: true})
	}
	return result
}

// FileTreeProposals creates deterministic scc-style fixture trees.
func FileTreeProposals(base uint64, namespace Namespace, maxCases, maxDepth int) []agents.WorkloadProposal {
	extensions := []string{"go", "py", "js", "rs"}
	seed := Seed(base, namespace)
	r := rand.New(rand.NewPCG(seed[0], seed[1]))
	if maxCases > len(extensions) {
		maxCases = len(extensions)
	}
	result := make([]agents.WorkloadProposal, 0, maxCases)
	for i := 0; i < maxCases; i++ {
		ext := extensions[r.IntN(len(extensions))]
		depth := 1 + r.IntN(max(1, maxDepth))
		parts := make([]string, depth)
		for j := range parts {
			parts[j] = fmt.Sprintf("d%d", j)
		}
		path := strings.Join(parts, "/") + "/sample." + ext
		result = append(result, agents.WorkloadProposal{Name: fmt.Sprintf("file-tree-%d", i), Arguments: []string{"."}, Fixtures: []agents.ProposedFixture{{Path: path, Content: "// deterministic fixture\n"}}, Tier: domain.TierPlausible, Provenance: "deterministic-file-tree", ScalingDimensions: map[string]int{"depth": depth}, ExpectedValid: true})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
