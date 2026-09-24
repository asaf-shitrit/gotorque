package campaign

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/profile"
	"github.com/stretchr/testify/require"
)

func gronEngine() *Engine {
	e := &Engine{}
	e.state.Inventory.Packages = []string{"github.com/tomnomnom/gron", "example.com/lib"}
	return e
}

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
	require.Equal(t, []string{"main.gron", "main.statements.Less", "example.com/lib.Render", "strings.Join"}, gronEngine().sampledHotNames(result))
}

// TestSampledHotNamesFallsBackToTopOfStack keeps the old list when the report
// carries no usable call paths, so attribution can only add evidence.
func TestSampledHotNamesFallsBackToTopOfStack(t *testing.T) {
	result := profile.SampleResult{Functions: []profile.Function{{Name: "main.statements.Less", Flat: "236"}, {Name: "strings.Join", Flat: "41"}}}
	require.Equal(t, []string{"main.statements.Less", "strings.Join"}, gronEngine().sampledHotNames(result))
}

func TestOwnSymbolMatchesTheModuleAndMainOnly(t *testing.T) {
	e := gronEngine()
	for symbol, want := range map[string]bool{
		"main.gron":                        true,
		"github.com/tomnomnom/gron.Format": true,
		"example.com/lib.(*T).Render":      true,
		"example.com/libx.Render":          false,
		"fmt.Fprintln":                     false,
		"write":                            false,
	} {
		require.Equal(t, want, e.ownSymbol(symbol), symbol)
	}
}

// TestResolveHotLocationsFoldsSymbolsSharingADeclaration: the value method and
// Go's generated pointer wrapper resolve to one declaration and fill one slot.
func TestResolveHotLocationsFoldsSymbolsSharingADeclaration(t *testing.T) {
	repo := t.TempDir()
	src := "package main\n\ntype statements []string\n\nfunc (ss statements) Less(a, b int) bool {\n\treturn ss[a] < ss[b]\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "statements.go"), []byte(src), 0o600))
	e := gronEngine()
	e.state.Repository = repo
	got := e.resolveHotLocations(context.Background(), "", []string{"main.statements.Less", "main.(*statements).Less", "strings.Join"})
	require.Equal(t, []string{"statements.go:5", "strings.Join"}, got)
}

func TestResolveHotLocationsStopsAtTheBudget(t *testing.T) {
	names := make([]string, 0, 2*hotFunctionBudget)
	for i := range 2 * hotFunctionBudget {
		names = append(names, "pkg.F"+string(rune('a'+i)))
	}
	e := gronEngine()
	e.state.Repository = t.TempDir()
	require.Len(t, e.resolveHotLocations(context.Background(), "", names), hotFunctionBudget)
}

// TestSamplingFallsBackToALongerStressSeed: a seed whose input is files runs
// only as long as its files make it, and the macOS sampler cannot attach to a
// process that exits at once. Discovery then samples the manifest's stress
// seed instead of giving up.
func TestSamplingFallsBackToALongerStressSeed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("exercises /usr/bin/sample")
	}
	e := &Engine{dir: t.TempDir()}
	e.state.BinaryPath = "/bin/sh"
	e.state.Manifest.Workloads.Seeds = []manifest.SeedWorkload{
		{ID: "quick", Tier: domain.TierRepresentative, Args: []string{"-c", "true"}},
		{ID: "medium", Tier: domain.TierPlausible, Args: []string{"-c", "true"}},
		{ID: "long", Tier: domain.TierStress, Args: []string{"-c", "i=0; while [ $i -lt 5000000 ]; do i=$((i+1)); done"}},
	}
	seed, result, err := e.sampleFirstLiving(context.Background())
	require.NoError(t, err)
	require.Equal(t, "long", seed.ID)
	require.NotEmpty(t, result.Functions)

	e.state.Manifest.Workloads.Seeds = e.state.Manifest.Workloads.Seeds[:2]
	_, _, err = e.sampleFirstLiving(context.Background())
	require.ErrorContains(t, err, "quick: ", "with no stress seed the first seed's failure is reported")
	require.NotContains(t, err.Error(), "medium", "only stress seeds are tried after the first")
}
