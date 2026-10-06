package discovery

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

const (
	hotMain  = "package main\n\nimport \"example.com/hot/hotlib\"\n\nfunc hot() int { return hotlib.Work(1e6) }\n\nfunc main() { hot() }\n"
	hotLib   = "package hotlib\n\nfunc Work(n int) int {\n\ts := 0\n\tfor i := range n {\n\t\ts += i * i % 7\n\t}\n\treturn s\n}\n"
	hotBench = "package hotlib\n\nimport \"testing\"\n\nvar sink []int\n\nfunc BenchmarkWork(b *testing.B) {\n\tfor range b.N {\n\t\tsink = make([]int, 1024)\n\t\t_ = Work(1e6)\n\t}\n}\n"
)

// moduleRepo is a module with a command (cmd/hot) that calls a library
// (hotlib), with or without a benchmark of the library.
func moduleRepo(t *testing.T, withBenchmark bool) string {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		"go.mod":           "module example.com/hot\n\ngo 1.22\n",
		"cmd/hot/main.go":  hotMain,
		"hotlib/hotlib.go": hotLib,
	}
	if withBenchmark {
		files["hotlib/hotlib_test.go"] = hotBench
	}
	writeTree(t, repo, files)
	return repo
}

func runInputs(t *testing.T, repo string) Inputs {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "hot")
	require.NoError(t, os.WriteFile(binary, []byte("not run: the sampler is scripted"), 0o600))
	return Inputs{
		BinaryPath: binary,
		Seeds:      []manifest.SeedWorkload{{ID: "seed", Args: []string{"-v"}}},
		Repository: repo,
		Build:      manifest.BuildTarget{Package: "./cmd/hot", Binary: "hot"},
		Packages:   []string{"example.com/hot", "example.com/hot/hotlib"},
		Dir:        t.TempDir(),
	}
}

func kinds(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		paths = append(paths, path)
		return err
	}))
	return paths
}

var testToolchain = toolchain.New(toolchain.Options{})

func TestRunSamplesTheTargetAndStillKeepsTheBenchmarkProfile(t *testing.T) {
	repo := moduleRepo(t, true)
	in := runInputs(t, repo)
	replay := profile.NewReplay(recordedBusy(t))
	before := treeOf(t, repo)

	ev, err := Run(context.Background(), in, replay, testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceTargetSample, ev.Source)
	require.Empty(t, ev.Events)
	require.Contains(t, ev.HotFunctions, "cmd/hot/main.go:5", "main.hot resolves to its declaration")
	require.Contains(t, ev.Weights, "main.hot")
	require.Equal(t, filepath.Join(in.Dir, "profile-sample", "sample-report.txt"), ev.ProfileSummaryPath)
	require.FileExists(t, ev.PGOProfilePath, "the PGO lane needs the benchmark profile even when sampling won")
	require.Empty(t, ev.AllocProfileSummaryPath)
	require.Equal(t, before, treeOf(t, repo), "profiling must not write into the checkout")

	requests := replay.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, []string{"-v"}, requests[0].Args)
}

func TestRunFallsBackToBenchmarksAndSaysWhySamplingFailed(t *testing.T) {
	repo := moduleRepo(t, true)
	in := runInputs(t, repo)
	before := treeOf(t, repo)
	ev, err := Run(context.Background(), in, profile.NewReplay(gone), testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceBenchmark, ev.Source)
	require.Equal(t, []string{"discovery_sample_failed"}, kinds(ev.Events))
	require.Contains(t, ev.Events[0].Message, "direct target sampling failed, falling back to benchmarks")
	require.Contains(t, ev.Events[0].Message, "target exited before sampling began")
	require.Contains(t, ev.HotFunctions, "hotlib/hotlib.go:3", "the benchmark profile resolves Work through pprof -list")
	require.FileExists(t, ev.PGOProfilePath)
	require.FileExists(t, ev.ProfileSummaryPath)
	require.Equal(t, before, treeOf(t, repo), "profiling must not write into the checkout")
}

func TestRunReportsNoSourceWhenNothingCanBeProfiled(t *testing.T) {
	in := runInputs(t, moduleRepo(t, false))
	ev, err := Run(context.Background(), in, profile.NewReplay(gone), testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceNone, ev.Source)
	require.Equal(t, []string{"discovery_sample_failed", "discovery_profile_skipped"}, kinds(ev.Events))
	require.Contains(t, ev.Events[1].Message, "direct target sampling unavailable")
	require.Contains(t, ev.Events[1].Message, "benchmark CPU profile unavailable")
	require.Empty(t, ev.HotFunctions)
	require.Empty(t, ev.PGOProfilePath)
}

func TestRunSkipsSamplingWhenThereIsNothingToSample(t *testing.T) {
	for name, tt := range map[string]struct {
		change func(*Inputs)
		reason string
	}{
		"no binary":       {func(in *Inputs) { in.BinaryPath = "" }, "no baseline binary"},
		"binary is gone":  {func(in *Inputs) { in.BinaryPath = filepath.Join(in.Dir, "absent") }, "baseline binary missing on disk"},
		"binary is a dir": {func(in *Inputs) { in.BinaryPath = in.Dir }, "baseline binary missing on disk"},
		"no seeds":        {func(in *Inputs) { in.Seeds = nil }, "manifest defines no seed workloads to sample"},
	} {
		t.Run(name, func(t *testing.T) {
			in := runInputs(t, moduleRepo(t, false))
			tt.change(&in)
			replay := profile.NewReplay(recordedBusy(t))
			ev, err := Run(context.Background(), in, replay, testToolchain)
			require.NoError(t, err)
			require.Empty(t, replay.Requests())
			require.Equal(t, SourceNone, ev.Source)
			require.Contains(t, ev.Events[0].Message, tt.reason)
		})
	}
}

func TestRunSamplesTheExplorersVariantsNextToTheSeed(t *testing.T) {
	in := runInputs(t, moduleRepo(t, false))
	var seen manifest.SeedWorkload
	in.Explore = func(_ context.Context, seed manifest.SeedWorkload) []manifest.SeedWorkload {
		seen = seed
		variant := seed
		variant.ID, variant.Args = seed.ID+" --stream", []string{"--stream", "-v"}
		return []manifest.SeedWorkload{variant}
	}
	replay := profile.NewReplay(recordedBusy(t))
	ev, err := Run(context.Background(), in, replay, testToolchain)
	require.NoError(t, err)
	require.Equal(t, "seed", seen.ID)
	require.Equal(t, SourceTargetSample, ev.Source)
	requests := replay.Requests()
	require.Len(t, requests, 2)
	require.Equal(t, []string{"--stream", "-v"}, requests[1].Args)
	require.Equal(t, filepath.Join(in.Dir, "profile-sample", "sample-report-1.txt"), requests[1].OutputPath)
}

func TestRunSurfacesTheLaddersEventsAndIsolationNotesInOrder(t *testing.T) {
	in := runInputs(t, moduleRepo(t, false))
	in.Explore = func(_ context.Context, seed manifest.SeedWorkload) []manifest.SeedWorkload {
		variant := seed
		variant.ID, variant.Stdin = "seed --stream", "{}"
		return []manifest.SeedWorkload{variant}
	}
	busy := recordedBusy(t)
	busy.IsolationNotes = []string{"memory limit not enforced"}
	// the seed samples; its variant ends too soon on the amplified document
	// and again on the per-line one.
	ev, err := Run(context.Background(), in, profile.NewReplay(busy, gone, gone), testToolchain)
	require.NoError(t, err)
	require.Equal(t, []string{"memory limit not enforced"}, ev.IsolationNotes)
	require.Equal(t, []string{"workload_sample_skipped"}, kinds(ev.Events))
	require.Equal(t, SourceTargetSample, ev.Source)
}

func TestRunAddsAnAllocationProfileUnderAMemoryObjective(t *testing.T) {
	repo := moduleRepo(t, true)
	in := runInputs(t, repo)
	in.MemoryObjective = true
	before := treeOf(t, repo)
	ev, err := Run(context.Background(), in, profile.NewReplay(recordedBusy(t)), testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceTargetSample+" + a benchmark alloc_space profile", ev.Source)
	require.Equal(t, []string{"discovery_alloc_profile_completed"}, kinds(ev.Events))
	require.FileExists(t, ev.AllocProfileSummaryPath)
	require.Equal(t, before, treeOf(t, repo), "allocation profiling must not write into the checkout")
}

func TestRunNotesWhenAMemoryObjectiveHasNoBenchmarks(t *testing.T) {
	in := runInputs(t, moduleRepo(t, false))
	in.MemoryObjective = true
	ev, err := Run(context.Background(), in, profile.NewReplay(recordedBusy(t)), testToolchain)
	require.NoError(t, err)
	require.Equal(t, SourceTargetSample, ev.Source, "no allocation source is named when there is none")
	require.Equal(t, []string{"discovery_alloc_profile_skipped"}, kinds(ev.Events))
}
