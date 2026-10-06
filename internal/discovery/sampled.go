package discovery

import (
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// ownSymbol reports whether a sampled frame belongs to the target: its package
// is one of the module's, or main, which is how a command's own functions are
// named in its binary whatever its import path.
func ownSymbol(packages []string, symbol string) bool {
	pkg := profile.SymbolPackage(symbol)
	return pkg == "main" || (pkg != "" && slices.Contains(packages, pkg))
}

// sampledHotNames ranks the target's own functions by the samples spent on
// their behalf, then fills any budget left with the sampler's top-of-stack
// frames, which is all discovery used to list.
//
// Top of stack alone says which frames were executing, not for whom. A Go CLI
// that spends its time printing is executing fmt and write, so its hot list
// was standard-library names no patch can touch, and the function doing the
// printing had almost no self time: gron's per-statement Fprintln loop, whose
// bufio fix was the only patch ever accepted on it, never appeared, so no
// analyst was ever asked about it. Credited with the calls it makes, it ranks
// first.
//
// It returns up to twice the budget, because resolving folds symbols that
// share a declaration and keeps resolving until the budget is filled.
//
// With explored workloads there are several samples; mergeAttributed weighs
// each as a whole, so a mode only a variant reaches ranks by its share of that
// variant's time rather than disappearing behind the seed.
func sampledHotNames(results []profile.SampleResult, own func(string) bool) []string {
	limit := 2 * hotFunctionBudget
	names := hotFunctionNames(mergeAttributed(results, own), limit)
	for _, result := range results {
		for _, name := range hotFunctionNames(result.Functions, limit) {
			if len(names) < limit && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// sampledHotNamesAndWeights is sampledHotNames plus the per-function weight
// that ranking (the same names, unranked and untruncated) carried, so a
// throwaway_result target's caller ranking (callers.go) has real profile
// hotness to rank by instead of call-site count alone.
func sampledHotNamesAndWeights(results []profile.SampleResult, own func(string) bool) ([]string, map[string]float64) {
	weights := HotFunctionWeights(mergeAttributed(results, own))
	for _, result := range results {
		weights = MergeWeights(weights, HotFunctionWeights(result.Functions))
	}
	return sampledHotNames(results, own), weights
}
