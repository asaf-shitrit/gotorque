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
	merged := mergeAttributed([]profile.SampleResult{seedSample, variantSample}, func(s string) bool { return strings.HasPrefix(s, "main.") })
	names := make([]string, 0, len(merged))
	for _, fn := range merged {
		names = append(names, fn.Name)
	}
	require.Equal(t, []string{"main.stream", "main.sort", "main.print"}, names)
}
