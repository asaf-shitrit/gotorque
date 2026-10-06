package discovery

import (
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// MergeAllocFirst puts every allocation-heavy location ahead of the
// CPU-derived hot list, preserving each list's own order, deduplicated and
// capped at budget: an allocator that never surfaced in the CPU profile still
// belongs in discovery's evidence, and one that did should not occupy two
// slots.
func MergeAllocFirst(cpu, alloc []string, budget int) []string {
	seen := make(map[string]bool, len(cpu)+len(alloc))
	merged := make([]string, 0, min(budget, len(cpu)+len(alloc)))
	for _, list := range [][]string{alloc, cpu} {
		for _, loc := range list {
			if seen[loc] || len(merged) == budget {
				continue
			}
			seen[loc] = true
			merged = append(merged, loc)
		}
	}
	return merged
}

// BenchmarkPackageOrder lists the packages worth profiling, target package
// first so a target that benchmarks its own command keeps that evidence, then
// the module's benchmark-bearing packages richest first. Each is a single
// package because the go command rejects -cpuprofile for more than one.
// BenchmarkPackageOrder tries the target package first, then the rest of the
// module. When the target builds from a nested module directory (ADR 0023),
// targetPackage is resolved relative to that directory, not to repository,
// and `go test <targetPackage>` from the repository root would name the
// wrong package or fail outright; it is left out of the root module's
// benchmark order in that case, and the nested module's own benchmarks, like
// its own tests, are not run (documented in docs/target-manifest.md).
//
// imports, when known, limits the rest to packages the target imports: a
// benchmark of code the CLI never runs profiles the wrong program. The
// held-out s2c target builds from klauspost/compress, whose richest benchmark
// package is flate, which s2c never imports; its campaign spent both attempts
// rewriting flate's StatelessDeflate.
func BenchmarkPackageOrder(repository, targetPackage, directory string, imports []string) []string {
	var order []string
	if directory == "" {
		order = append(order, targetPackage)
	}
	for _, pkg := range profile.BenchmarkPackages(repository) {
		if pkg != targetPackage && (imports == nil || slices.Contains(imports, pkg)) {
			order = append(order, pkg)
		}
	}
	return order
}
