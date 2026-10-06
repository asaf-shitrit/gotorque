package discovery

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

var gronPackages = []string{"github.com/tomnomnom/gron", "example.com/lib"}

func gronOwn(symbol string) bool { return ownSymbol(gronPackages, symbol) }

// TestSampledHotNamesRanksOwnFunctionsByAttributedSamples: the function that
// makes the expensive library calls outranks the frames that execute them, and
// the budget is topped up from the old top-of-stack list without repeats.
func TestSampledHotNamesRanksOwnFunctionsByAttributedSamples(t *testing.T) {
	result := profile.SampleResult{
		Functions: []profile.Function{{Name: "write", Flat: "383"}, {Name: "main.statements.Less", Flat: "236"}, {Name: "strings.Join", Flat: "41"}},
		Stacks: []profile.Stack{
			{Frames: []string{"write", "syscall.write", "fmt.Fprintln", "main.gron", "main.main"}, Weight: 400},
			{Frames: []string{"main.statements.Less", "sort.pdqsort"}, Weight: 236},
			{Frames: []string{"strings.Join", "example.com/lib.Render", "main.gron"}, Weight: 41},
		},
	}
	require.Equal(t, []string{"main.gron", "main.statements.Less", "example.com/lib.Render", "strings.Join"}, sampledHotNames([]profile.SampleResult{result}, gronOwn))
}

// TestSampledHotNamesFallsBackToTopOfStack keeps the old list when the report
// carries no usable call paths, so attribution can only add evidence.
func TestSampledHotNamesFallsBackToTopOfStack(t *testing.T) {
	result := profile.SampleResult{Functions: []profile.Function{{Name: "main.statements.Less", Flat: "236"}, {Name: "strings.Join", Flat: "41"}}}
	require.Equal(t, []string{"main.statements.Less", "strings.Join"}, sampledHotNames([]profile.SampleResult{result}, gronOwn))
}

func TestSampledHotNamesAndWeightsKeepsEveryActionableFunctionsWeight(t *testing.T) {
	result := profile.SampleResult{
		Functions: []profile.Function{{Name: "main.statements.Less", Flat: "236"}},
		Stacks:    []profile.Stack{{Frames: []string{"main.statements.Less", "main.gron"}, Weight: 236}},
	}
	names, weights := sampledHotNamesAndWeights([]profile.SampleResult{result}, gronOwn)
	require.Equal(t, []string{"main.statements.Less"}, names)
	require.Contains(t, weights, "main.statements.Less")
	require.NotContains(t, weights, "write")
}

func TestOwnSymbolMatchesTheModuleAndMainOnly(t *testing.T) {
	for symbol, want := range map[string]bool{
		"main.gron":                        true,
		"github.com/tomnomnom/gron.Format": true,
		"example.com/lib.(*T).Render":      true,
		"example.com/libx.Render":          false,
		"fmt.Fprintln":                     false,
		"write":                            false,
	} {
		require.Equal(t, want, gronOwn(symbol), symbol)
	}
}
