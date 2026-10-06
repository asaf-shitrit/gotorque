package discovery

import (
	"sort"
	"strconv"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// MergeAttributed sums each sample's attributed weights as fractions of that
// sample's total, so every sampled workload counts equally whatever its length.
func MergeAttributed(results []profile.SampleResult, own func(string) bool) []profile.Function {
	weights := map[string]float64{}
	for _, result := range results {
		attributed := profile.AttributeToOwn(result.Stacks, own)
		total := 0.0
		for _, fn := range attributed {
			total += float64(atoiOrZero(fn.Flat))
		}
		if total == 0 {
			continue
		}
		for _, fn := range attributed {
			weights[fn.Name] += float64(atoiOrZero(fn.Flat)) / total
		}
	}
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if weights[names[i]] != weights[names[j]] {
			return weights[names[i]] > weights[names[j]]
		}
		return names[i] < names[j]
	})
	merged := make([]profile.Function, 0, len(names))
	for _, name := range names {
		merged = append(merged, profile.Function{Name: name, Flat: strconv.Itoa(int(weights[name] * 10000))})
	}
	return merged
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func isTestEntryPoint(segment string) bool {
	for _, prefix := range []string{"Benchmark", "Test", "Fuzz", "Example"} {
		if strings.HasPrefix(segment, prefix) {
			return true
		}
	}
	return false
}

func lastSegment(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 && idx < len(name)-1 {
		return name[idx+1:]
	}
	return name
}

const (
	// hotFunctionBudget caps how many actionable functions reach the agents.
	HotFunctionBudget = 15
	// hotFunctionScanDepth is how many profile nodes are summarized to fill
	// that budget. A Go CPU profile's hottest nodes are overwhelmingly
	// runtime scheduler and allocator frames, so scanning only as deep as the
	// budget yields a handful of module functions and wastes the rest of the
	// budget on frames no patch can touch.
	HotFunctionScanDepth = 4 * HotFunctionBudget
)

// HotFunctionNames extracts deduplicated function names from a parsed pprof
// top summary, skipping runtime frames that never belong to the target.
func HotFunctionNames(functions []profile.Function, limit int) []string {
	names := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, fn := range functions {
		name := strings.TrimSpace(fn.Name)
		if name == "" || strings.HasPrefix(name, "runtime.") || seen[name] || !actionableSymbol(name) {
			continue
		}
		seen[name] = true
		names = append(names, name)
		if len(names) == limit {
			break
		}
	}
	return names
}

// HotFunctionWeights maps every actionable function name in functions to a
// comparable hotness score, uncapped by hotFunctionBudget: pprof's cumulative
// percent, falling back to flat percent, falling back to the sample-based
// path's raw attributed count (mergeAttributed sets Flat only, both percents
// zero). It feeds a throwaway_result target's caller ranking (callers.go),
// which needs real weight for more functions than the truncated hot list
// keeps: dasel's own callers mostly tie at one call site each, so the profile
// is the only thing that can tell them apart (ADR 0027's addendum).
func HotFunctionWeights(functions []profile.Function) map[string]float64 {
	out := map[string]float64{}
	for _, fn := range functions {
		name := strings.TrimSpace(fn.Name)
		if name == "" || strings.HasPrefix(name, "runtime.") || !actionableSymbol(name) {
			continue
		}
		w := fn.CumulativePercent
		if w == 0 {
			w = fn.FlatPercent
		}
		if w == 0 {
			w = float64(atoiOrZero(fn.Flat))
		}
		if existing, ok := out[name]; !ok || w > existing {
			out[name] = w
		}
	}
	return out
}

// MergeWeights folds src into dst, keeping the higher weight on a name both
// carry, and returns dst (built if nil). Several profiles can name the same
// function (CPU and allocation passes, seed and explored workloads); keeping
// the max is a conservative merge that never lets a smaller pass understate a
// function's true hotness.
func MergeWeights(dst, src map[string]float64) map[string]float64 {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]float64, len(src))
	}
	for name, w := range src {
		if existing, ok := dst[name]; !ok || w > existing {
			dst[name] = w
		}
	}
	return dst
}

// actionableSymbol rejects frames no source change can address.
//
// The OS sampler reports kernel and libc symbols (__psynch_cvwait, kevent,
// nanosleep) that describe a process waiting, not computing. Go symbols always
// carry a package qualifier, so the absence of a dot is a reliable
// discriminator for those.
//
// Benchmark CPU profiles additionally carry the harness that drove them
// (testing.(*B).runN and friends). Those frames are an artifact of how the
// measurement was taken rather than of the program under test, and agents
// otherwise rank them as top hot paths and reason about them as target code.
func actionableSymbol(name string) bool {
	if strings.HasPrefix(name, "_") {
		return false
	}
	if strings.HasPrefix(name, "testing.") {
		return false
	}
	// The module's own benchmark, test and fuzz entry points are sampled too
	// when the profile comes from its test binary. They are measurement
	// scaffolding, not the program, and a patch to one optimizes nothing the
	// target ships.
	if isTestEntryPoint(lastSegment(name)) {
		return false
	}
	return strings.Contains(name, ".")
}
