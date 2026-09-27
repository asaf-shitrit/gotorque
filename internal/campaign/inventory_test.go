package campaign

import (
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
	inv, err := decodeInventory(out)
	require.NoError(t, err)
	require.Equal(t, []string{"example.com/m/cmd/mlr", "example.com/m/pkg/lib"}, inv.Packages)
	require.Equal(t, []string{"example.com/m/cmd/mlr"}, inv.Commands)
	require.Equal(t, []string{"example.com/m/scripts/perf: C source files not allowed when not using cgo or SWIG: catc.c"}, inv.Unloadable)

	_, err = decodeInventory([]byte("{not json"))
	require.ErrorContains(t, err, "decode go list")
}

func TestInventoryReportListsUnloadablePackages(t *testing.T) {
	var b strings.Builder
	writeInventory(&b, State{Inventory: Inventory{Packages: []string{"a"}, Unloadable: []string{"x: broken"}}})
	require.Contains(t, b.String(), "1 package(s) could not be loaded and were left out")
	require.Contains(t, b.String(), "- `x: broken`")
}
