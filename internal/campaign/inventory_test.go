package campaign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
