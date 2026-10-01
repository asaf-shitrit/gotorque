package campaign

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/stretchr/testify/require"
)

// TestDecodeInventoryLeavesOutUnloadablePackages pins the fix for miller: its
// scripts/perf directories hold C and C++ sources that cannot build without
// cgo, and go list reporting them failed the whole campaign at inventory.
func TestDecodeInventoryLeavesOutUnloadablePackages(t *testing.T) {
	out := []byte(`{"ImportPath":"example.com/m/cmd/mlr","Name":"main"}
{"ImportPath":"example.com/m/pkg/lib","Name":"lib"}
{"ImportPath":"example.com/m/scripts/perf","Name":"","Error":{"Err":"C source files not allowed when not using cgo or SWIG: catc.c\nmore"}}
`)
	inv, err := decodeInventory(out, "", "")
	require.NoError(t, err)
	require.Equal(t, []string{"example.com/m/cmd/mlr", "example.com/m/pkg/lib"}, inv.Packages)
	require.Equal(t, []string{"example.com/m/cmd/mlr"}, inv.Commands)
	require.Equal(t, []string{"example.com/m/scripts/perf: C source files not allowed when not using cgo or SWIG: catc.c"}, inv.Unloadable)

	_, err = decodeInventory([]byte("{not json"), "", "")
	require.ErrorContains(t, err, "decode go list")
}

func TestInventoryReportListsUnloadablePackages(t *testing.T) {
	var b strings.Builder
	writeInventory(&b, State{Inventory: Inventory{Packages: []string{"a"}, Unloadable: []string{"x: broken"}}})
	require.Contains(t, b.String(), "1 package(s) could not be loaded and were left out")
	require.Contains(t, b.String(), "- `x: broken`")
}

// TestBaselineBinariesPresent pins the resume fix for a cleared builds
// directory: a completed build step whose binaries are gone is run again.
func TestBaselineBinariesPresent(t *testing.T) {
	dir := t.TempDir()
	release, coverage := filepath.Join(dir, "release"), filepath.Join(dir, "coverage")
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.True(t, baselineBinariesPresent(State{BinaryPath: release}))
	require.False(t, baselineBinariesPresent(State{BinaryPath: release, DiscoveryBinaryPath: coverage}))
	require.NoError(t, os.WriteFile(coverage, nil, 0o600))
	require.True(t, baselineBinariesPresent(State{BinaryPath: release, DiscoveryBinaryPath: coverage}))
	require.True(t, baselineBinariesPresent(State{}), "a campaign that never built has nothing to check")
}

// TestInventoryRecordsTheModulePackagesTheTargetImports: the held-out s2c
// target builds ./s2/cmd/s2c from klauspost/compress, which imports s2 but not
// flate. Benchmark profiling may only fall back to packages the target runs.
func TestInventoryRecordsTheModulePackagesTheTargetImports(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{"s2/cmd/s2c", "s2", "flate", "internal/snapref"} {
		require.NoError(t, os.MkdirAll(filepath.Join(repo, d), 0o700))
	}
	out := []byte(`{"ImportPath":"example.com/c/flate","Name":"flate","Dir":"` + filepath.Join(repo, "flate") + `"}
{"ImportPath":"example.com/c/internal/snapref","Name":"snapref","Dir":"` + filepath.Join(repo, "internal/snapref") + `"}
{"ImportPath":"example.com/c/s2","Name":"s2","Dir":"` + filepath.Join(repo, "s2") + `","Deps":["bytes","example.com/c/internal/snapref"]}
{"ImportPath":"example.com/c/s2/cmd/s2c","Name":"main","Dir":"` + filepath.Join(repo, "s2/cmd/s2c") + `","Deps":["bytes","example.com/c/internal/snapref","example.com/c/s2","flag"]}
`)
	inv, err := decodeInventory(out, repo, filepath.Join(repo, "./s2/cmd/s2c"))
	require.NoError(t, err)
	require.Equal(t, []string{"./internal/snapref", "./s2", "./s2/cmd/s2c"}, inv.TargetImports)

	inv, err = decodeInventory(out, repo, filepath.Join(repo, "nested/cmd"))
	require.NoError(t, err)
	require.Nil(t, inv.TargetImports, "a build package go list did not report leaves the fallback unfiltered")
}

// TestBenchmarkOrderKeepsOnlyImportedPackages: with the imports known, a
// benchmark-rich package the target never imports is not profiled.
func TestBenchmarkOrderKeepsOnlyImportedPackages(t *testing.T) {
	repo := t.TempDir()
	for pkg, n := range map[string]int{"flate": 3, "s2": 1} {
		require.NoError(t, os.MkdirAll(filepath.Join(repo, pkg), 0o700))
		var body strings.Builder
		body.WriteString("package " + pkg + "\n\nimport \"testing\"\n")
		for i := range n {
			body.WriteString("\nfunc BenchmarkX" + strconv.Itoa(i) + "(b *testing.B) {}\n")
		}
		require.NoError(t, os.WriteFile(filepath.Join(repo, pkg, "x_test.go"), []byte(body.String()), 0o600))
	}
	require.Equal(t, []string{"./s2/cmd/s2c"}, benchmarkPackageOrder(repo, "./s2/cmd/s2c", "", []string{"./s2/cmd/s2c"}), "no imported package has benchmarks")
	require.Equal(t, []string{"./s2/cmd/s2c", "./s2"}, benchmarkPackageOrder(repo, "./s2/cmd/s2c", "", []string{"./s2", "./s2/cmd/s2c"}))
	require.Equal(t, []string{"./s2/cmd/s2c", "./flate", "./s2"}, benchmarkPackageOrder(repo, "./s2/cmd/s2c", "", nil), "unknown imports keep the old order")
}

// TestAmplifyRepeatsScalesOnlyDeclaredRepeatableInputs: a fixture or stdin the
// manifest wrote with a repeat count grows to about amplificationTarget for
// the sampled run; an input without one (a script) is left alone, and a count
// already larger is kept.
func TestAmplifyRepeatsScalesOnlyDeclaredRepeatableInputs(t *testing.T) {
	block := strings.Repeat("x", 100)
	seed := manifest.SeedWorkload{
		ID:    "s",
		Stdin: block, StdinHeader: "h\n", StdinRepeat: 3,
		Files: []manifest.FixtureFile{
			{Path: "rows.csv", Header: "id\n", Content: block, Repeat: 10},
			{Path: "bench.lua", Content: "print(1)\n"},
			{Path: "huge.txt", Content: block, Repeat: 1 << 20},
		},
	}
	got := amplifyRepeats(seed, amplificationTarget)
	want := (amplificationTarget - 3) / 100
	require.Equal(t, want, got.Files[0].Repeat)
	require.Equal(t, 0, got.Files[1].Repeat, "a script has no repeat and is not repeated")
	require.Equal(t, 1<<20, got.Files[2].Repeat, "a count already past the target is kept")
	require.Equal(t, (amplificationTarget-2)/100, got.StdinRepeat)
	require.Equal(t, 10, seed.Files[0].Repeat, "the manifest's seed is not modified")

	plain := amplifyRepeats(manifest.SeedWorkload{Stdin: "{}"}, amplificationTarget)
	require.Equal(t, 0, plain.StdinRepeat)
	require.Equal(t, 5, scaledRepeat(0, 0, 5, amplificationTarget), "an empty block cannot be scaled")
}

// TestSamplingRetryAppliesOnlyToRepeatableInputs: the larger retry only makes
// sense for inputs the manifest declares repeatable; a script cannot grow.
func TestSamplingRetryAppliesOnlyToRepeatableInputs(t *testing.T) {
	require.True(t, hasRepeatableInput(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "a,b\n", Repeat: 10}}}))
	require.True(t, hasRepeatableInput(manifest.SeedWorkload{Stdin: "x\n", StdinRepeat: 2}))
	require.False(t, hasRepeatableInput(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "print(1)\n"}}}))

	big := amplifyRepeats(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: strings.Repeat("x", 100), Repeat: 10}}}, retryAmplificationTarget)
	require.Equal(t, retryAmplificationTarget/100, big.Files[0].Repeat)
}

// TestRetriesLargerOnATargetThatEndedTooSoon: a target dead before the
// liveness check, or alive for it and gone before any sample landed (held-out
// csvq's empty call graph), retries larger when its inputs can grow.
func TestRetriesLargerOnATargetThatEndedTooSoon(t *testing.T) {
	repeatable := manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "a,b\n", Repeat: 10}}}
	script := manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "print(1)\n"}}}
	require.True(t, retriesLarger(profile.ErrTargetExitedEarly, repeatable))
	require.True(t, retriesLarger(fmt.Errorf("sample: %w", profile.ErrNoFrames), repeatable))
	require.False(t, retriesLarger(profile.ErrNoFrames, script), "a script cannot grow")
	require.False(t, retriesLarger(errors.New("sample tool unavailable"), repeatable))
	require.False(t, retriesLarger(nil, repeatable))
}
