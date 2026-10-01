package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/jev"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/stretchr/testify/require"
)

// writerRepo is fzf's shape: an options constructor whose Printer closure
// prints each record, a small caller, and a caller over the classification
// limit.
func writerRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	huge := "func Run() {\n" + strings.Repeat("\t_ = defaultOptions().Printer\n", maxCauseSourceBytes/28+1) + "}\n"
	for rel, src := range map[string]string{
		"go.mod":     "module example.com/fz\n",
		"options.go": "package main\n\nimport \"fmt\"\n\ntype Options struct{ Printer func(string) }\n\nfunc defaultOptions() *Options {\n\treturn &Options{Printer: func(s string) { fmt.Println(s) }}\n}\n",
		"filter.go":  "package main\n\nfunc filter(items []string) {\n\tfor _, s := range items {\n\t\tdefaultOptions().Printer(s)\n\t}\n}\n",
		"core.go":    "package main\n\n" + huge,
		"main.go":    "package main\n\nfunc main() { Run() }\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o600))
	}
	return repo
}

func TestUnbufferedWriteTargetsGoFirstAsAFunctionSet(t *testing.T) {
	repo := writerRepo(t)
	result := agents.AnalystResult{
		Targets: []agents.Target{
			{Location: "options.go:7", Function: "defaultOptions", Cause: string(jev.CauseUnbufferedIO)},
			{Location: "filter.go:3", Function: "filter", Cause: string(jev.CauseAlloc)},
		},
		CandidateHypotheses: []string{"jev"},
	}
	addUnbufferedWriteTargets(repo, []orchestrator.UnbufferedWrite{
		{Location: "options.go:7", Share: 0.95, Callers: []string{"core.go:3", "filter.go:3", "nowhere.go:1"}},
		{Location: "missing.go:4", Share: 0.9},
	}, &result)

	require.Len(t, result.Targets, 2, "Jev's unbuffered_io at the same location is dropped")
	got := result.Targets[0]
	require.Equal(t, causeUnbufferedWrites, got.Cause)
	require.Equal(t, "defaultOptions", got.Function)
	require.True(t, got.IsFunctionSet())
	require.Equal(t, []agents.FunctionRef{{Name: "filter", Location: "filter.go:3"}}, got.Callers, "Run is over the size limit")
	require.Contains(t, got.Remedy, "95%")
	require.Contains(t, got.Remedy, "Run, filter")
	require.Equal(t, jev.CauseAlloc, jev.Cause(result.Targets[1].Cause))
	require.Equal(t, []string{got.Remedy, "jev"}, result.CandidateHypotheses)
	require.Len(t, result.HotPaths, 2)
}

func TestUnbufferedWriteTargetWithoutCallersIsOneFunction(t *testing.T) {
	result := agents.AnalystResult{}
	addUnbufferedWriteTargets(writerRepo(t), []orchestrator.UnbufferedWrite{{Location: "filter.go:4", Share: 0.6}}, &result)
	require.Len(t, result.Targets, 1)
	require.False(t, result.Targets[0].IsFunctionSet())
	require.Equal(t, "filter", result.Targets[0].Function)

	empty := agents.AnalystResult{Targets: []agents.Target{{Location: "a.go:1"}}}
	addUnbufferedWriteTargets(t.TempDir(), []orchestrator.UnbufferedWrite{{Location: "a.go:1", Share: 1}}, &empty)
	require.Len(t, empty.Targets, 1, "evidence at no readable declaration adds nothing")
}

// TestUnbufferedWritesKeepsHotListSites resolves the sample's write sites to
// hot-list locations and callers, dropping a site discovery did not list.
func TestUnbufferedWritesKeepsHotListSites(t *testing.T) {
	repo := writerRepo(t)
	e := &Engine{}
	e.state.Repository = repo
	e.state.Inventory.Packages = []string{"example.com/fz"}
	e.state.DiscoveryHotFunctions = []string{"options.go:7", "filter.go:3"}
	write := []string{"syscall.write", "fmt.Fprintln"}
	results := []profile.SampleResult{
		{Stacks: []profile.Stack{
			{Frames: append(append([]string{}, write...), "main.defaultOptions.func1", "main.filter", "main.Run", "main.main"), Weight: 80},
			{Frames: append(append([]string{}, write...), "main.main"), Weight: 50},
		}},
		{Stacks: []profile.Stack{
			{Frames: append(append([]string{}, write...), "main.defaultOptions.func1", "main.filter"), Weight: 10},
			{Frames: []string{"main.defaultOptions.func1"}, Weight: 10},
		}},
	}
	got := e.unbufferedWrites(context.Background(), results)
	require.Equal(t, []orchestrator.UnbufferedWrite{{Location: "options.go:7", Share: 1, Callers: []string{"filter.go:3"}}}, got)
}

func TestReportLabelsCodeDerivedTargets(t *testing.T) {
	require.Equal(t, "code-derived", targetEvidence(agents.Target{Cause: causeUnbufferedWrites}))
	require.Equal(t, "code-derived", targetEvidence(agents.Target{Cause: causeThrowawayResult}))
	require.Equal(t, "+3.3 sd", targetEvidence(agents.Target{Cause: "unbuffered_io", Z: 3.3}))
}
