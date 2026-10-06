package discovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// TestUnbufferedWritesKeepsHotListSites resolves the sample's write sites to
// hot-list locations and callers, dropping a site discovery did not list.
func TestUnbufferedWritesKeepsHotListSites(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod":     "module example.com/fz\n",
		"options.go": "package main\n\nimport \"fmt\"\n\ntype Options struct{ Printer func(string) }\n\nfunc defaultOptions() *Options {\n\treturn &Options{Printer: func(s string) { fmt.Println(s) }}\n}\n",
		"filter.go":  "package main\n\nfunc filter(items []string) {\n\tfor _, s := range items {\n\t\tdefaultOptions().Printer(s)\n\t}\n}\n",
	})
	own := func(symbol string) bool { return ownSymbol([]string{"example.com/fz"}, symbol) }
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
	hot := []string{"options.go:7", "filter.go:3"}
	got := unbufferedWrites(context.Background(), locator{repository: repo}, hot, own, results)
	require.Equal(t, []orchestrator.UnbufferedWrite{{Location: "options.go:7", Share: 1, Callers: []string{"filter.go:3"}}}, got)

	require.Empty(t, unbufferedWrites(context.Background(), locator{repository: repo}, []string{"filter.go:3"}, own, results), "a write site outside the hot list is dropped")
	require.Empty(t, unbufferedWrites(context.Background(), locator{repository: repo}, nil, own, results), "with no hot list there is nothing to keep")
}
