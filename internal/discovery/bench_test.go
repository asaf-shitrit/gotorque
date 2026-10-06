package discovery

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestMergeAllocFirstPrefersAllocatorsWithoutDroppingCPUEvidence(t *testing.T) {
	cpu := []string{"a.go:1", "b.go:2", "c.go:3"}
	alloc := []string{"c.go:3", "d.go:4"}

	got := mergeAllocFirst(cpu, alloc, 4)
	require.Equal(t, []string{"c.go:3", "d.go:4", "a.go:1", "b.go:2"}, got, "allocators lead, deduplicated, then the rest of the CPU list")

	require.Equal(t, []string{"c.go:3", "d.go:4"}, mergeAllocFirst(cpu, alloc, 2), "budget still caps the merged list")
}
