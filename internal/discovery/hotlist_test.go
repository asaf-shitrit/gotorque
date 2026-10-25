package discovery

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

func TestHotFunctionNamesDropsNonActionableFrames(t *testing.T) {
	// Frames as the macOS sampler reports them for a CLI that spends most of
	// its wall time blocked: only the Go symbols can be patched.
	functions := []profile.Function{
		{Name: "__psynch_cvwait"},
		{Name: "github.com/itchyny/gojq/cli.newJSONInputIter.func1"},
		{Name: "_pthread_cond_wait"},
		{Name: "kevent"},
		{Name: "encoding/json/v2.Unmarshal"},
		{Name: "nanosleep"},
		{Name: "runtime.mallocgc"},
		{Name: "testing.(*B).runN"},
		{Name: "github.com/tomnomnom/gron.BenchmarkBigJSON"},
		{Name: "github.com/tomnomnom/gron.TestFill"},
	}
	got := hotFunctionNames(functions, 15)
	want := []string{
		"github.com/itchyny/gojq/cli.newJSONInputIter.func1",
		"encoding/json/v2.Unmarshal",
	}
	if !slices.Equal(got, want) {
		t.Errorf("hotFunctionNames() = %v, want %v", got, want)
	}
}

func TestHotFunctionNamesSkipsRuntimeAndDeduplicates(t *testing.T) {
	functions := []profile.Function{
		{Name: "runtime.schedule"},
		{Name: " main.handle "},
		{Name: "main.handle"},
		{Name: ""},
		{Name: "main.parse"},
	}
	got := hotFunctionNames(functions, 15)
	require.Equal(t, []string{"main.handle", "main.parse"}, got)
	require.Empty(t, hotFunctionNames(nil, 15))
	capped := hotFunctionNames([]profile.Function{{Name: "main.a"}, {Name: "main.b"}}, 1)
	require.Equal(t, []string{"main.a"}, capped)
}

// TestMergedSamplesWeighEachWorkloadEqually: a mode only one variant reaches
// ranks by its share of that variant's time, not behind the seed's totals.
func TestMergedSamplesWeighEachWorkloadEqually(t *testing.T) {
	seedSample := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.sort"}, Weight: 900}, {Frames: []string{"main.print"}, Weight: 100}}}
	variantSample := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.stream"}, Weight: 40}}}
	merged := mergeAttributed([]weightedSample{{Result: seedSample, Weight: 1}, {Result: variantSample, Weight: 1}}, func(s string) bool { return strings.HasPrefix(s, "main.") })
	names := make([]string, 0, len(merged))
	for _, fn := range merged {
		names = append(names, fn.Name)
	}
	require.Equal(t, []string{"main.stream", "main.sort", "main.print"}, names)
}

// TestMergedSamplesWeighASeedByItsTierWeight: a representative seed's reading
// can accept a candidate and a plausible seed's cannot, so the manifest's tier
// weight decides how much of the merged hot list each sample's shape owns. A
// function carrying half of the representative sample outranks one carrying
// half of the plausible sample; equal weighting ties them and ranks by name.
func TestMergedSamplesWeighASeedByItsTierWeight(t *testing.T) {
	representative := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.eval"}, Weight: 50}, {Frames: []string{"main.walk"}, Weight: 50}}}
	plausible := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.jit"}, Weight: 50}, {Frames: []string{"main.eval"}, Weight: 50}}}
	own := func(s string) bool { return strings.HasPrefix(s, "main.") }
	weighted := mergeAttributed([]weightedSample{{Result: representative, Weight: 1}, {Result: plausible, Weight: 0.5}}, own)
	require.Equal(t, []string{"main.eval", "main.walk", "main.jit"}, namesOf(weighted))

	equal := mergeAttributed([]weightedSample{{Result: representative, Weight: 1}, {Result: plausible, Weight: 1}}, own)
	require.Equal(t, []string{"main.eval", "main.jit", "main.walk"}, namesOf(equal), "equal weights tie these three, ranked by name")
}

// TestMergedSamplesCountEquallyWhenNoTierCarriesAWeight: a manifest that
// weights every tier at zero (or an Inputs with no tier policy) must still
// rank by sampled time, not fall back to ranking by name.
func TestMergedSamplesCountEquallyWhenNoTierCarriesAWeight(t *testing.T) {
	heavy := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.zloop"}, Weight: 900}, {Frames: []string{"main.asort"}, Weight: 100}}}
	light := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.mcheck"}, Weight: 30}}}
	merged := mergeAttributed([]weightedSample{{Result: heavy}, {Result: light}}, func(s string) bool { return strings.HasPrefix(s, "main.") })
	require.Equal(t, []string{"main.mcheck", "main.zloop", "main.asort"}, namesOf(merged), "the short sample counts as one workload, not by its sample count")
}

func namesOf(functions []profile.Function) []string {
	names := make([]string, 0, len(functions))
	for _, fn := range functions {
		names = append(names, fn.Name)
	}
	return names
}
