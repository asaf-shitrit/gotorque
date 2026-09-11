package profile

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var benchmarkDecl = regexp.MustCompile(`(?m)^func Benchmark[A-Za-z0-9_]*\(`)

// BenchmarkPackages lists the relative package paths under root whose test
// files declare benchmarks, most benchmarks first and ties broken
// lexicographically so repeated campaigns profile the same package.
//
// Callers need this because `go test` rejects -cpuprofile with more than one
// package, so "./..." is never a usable profiling target. A CLI's command
// package typically declares no benchmarks while the library packages it
// drives do, and picking the richest package is what lets those targets be
// profiled at all instead of degrading to an OS sampler.
func BenchmarkPackages(root string) []string {
	counts := map[string]int{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return skipIgnoredDir(d.Name())
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		found := len(benchmarkDecl.FindAll(data, -1))
		if found == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return nil
		}
		counts[packagePath(rel)] += found
		return nil
	})

	packages := make([]string, 0, len(counts))
	for pkg := range counts {
		packages = append(packages, pkg)
	}
	sort.Slice(packages, func(i, j int) bool {
		if counts[packages[i]] != counts[packages[j]] {
			return counts[packages[i]] > counts[packages[j]]
		}
		return packages[i] < packages[j]
	})
	return packages
}

// packagePath renders a filepath-relative directory as the "./pkg" form the
// go command accepts.
func packagePath(rel string) string {
	slashed := filepath.ToSlash(rel)
	if slashed == "." || slashed == "" {
		return "."
	}
	return "./" + slashed
}
