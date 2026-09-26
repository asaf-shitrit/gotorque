package campaign

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
)

// throwawaySource mirrors the dasel pattern the throwaway_result signal
// targets: (*T).Get always returns a fresh *X (a package-level constructor
// that itself returns &X{...}), and four callers in the same package use the
// result four different ways -- three only read a field or call one cheap
// method on it and drop it, and one returns it, so it escapes.
const throwawaySource = `package pkg

type X struct{ v int }

func NewX(v int) *X { return &X{v: v} }

func (x *X) Positive() bool { return x.v > 0 }

type T struct{ x int }

func (t *T) Get() *X {
	return NewX(t.x)
}

func (t *T) IsPositive() bool {
	return t.Get().Positive()
}

func (t *T) Value() int {
	g := t.Get()
	return g.v
}

func (t *T) Sum() int {
	h := t.Get()
	return h.v + h.v
}

func (t *T) Escapes() *X {
	return t.Get()
}
`

func writeThrowawayFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte(throwawaySource), 0o600))
	return repo
}

// TestThrowawaySignalFiresOnConsumingCallersOnly is the replay gate's unit
// fixture (gotorque task item 5a): a callee returning a fresh allocation with
// four callers, three consuming and one escaping. The signal must fire, and
// the caller set it reports must be exactly the three consuming callers, in
// their (*T) method names, never the escaping one.
func TestThrowawaySignalFiresOnConsumingCallersOnly(t *testing.T) {
	repo := writeThrowawayFixture(t)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get"}

	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.Fresh, "Get always returns NewX(...), a fresh allocation")
	require.True(t, analysis.signal(), "3 of 4 call sites are consumed, so the signal should fire")

	callers := analysis.consumingCallers(nil)
	require.Len(t, callers, 3)
	names := make([]string, 0, len(callers))
	for _, c := range callers {
		names = append(names, c.Caller)
	}
	require.ElementsMatch(t, names, []string{"(*T).IsPositive", "(*T).Value", "(*T).Sum"})
	for _, c := range callers {
		require.Equal(t, usageConsumed, c.Usage)
	}

	var sawEscape bool
	for _, s := range analysis.Sites {
		if s.Caller == "(*T).Escapes" {
			sawEscape = true
			require.Equal(t, usageEscapes, s.Usage)
		}
	}
	require.True(t, sawEscape, "Escapes must be classified, and as escaping")
}

// TestThrowawayTargetCapsAndRemedies checks the Target built from the signal:
// its Functions set carries the three consuming callers (never Escapes), and
// its cause and remedy are the code-derived ones, not a Jev cause.
func TestThrowawayTargetCapsAndRemedies(t *testing.T) {
	repo := writeThrowawayFixture(t)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get", Location: "pkg.go:14"}

	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.signal())

	target := throwawayTarget(site, analysis, nil)
	require.Equal(t, causeThrowawayResult, target.Cause)
	require.Equal(t, "(*T).Get", target.Function)
	require.Equal(t, "pkg.go:14", target.Location)
	require.Contains(t, target.Remedy, "(*T).Get")

	fns := target.Callers
	names := make([]string, 0, len(fns))
	for _, f := range fns {
		names = append(names, f.Name)
		require.NotEmpty(t, f.Location)
	}
	require.ElementsMatch(t, names, []string{"(*T).IsPositive", "(*T).Value", "(*T).Sum"})
}

// TestThrowawaySignalRequiresAFreshAllocation: a callee whose return is not a
// fresh allocation never fires, whatever its callers do.
func TestThrowawaySignalRequiresAFreshAllocation(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	src := `package pkg

type T struct{ cached *X }
type X struct{ v int }
func (x *X) Positive() bool { return x.v > 0 }

func (t *T) Get() *X {
	return t.cached
}

func (t *T) IsPositive() bool { return t.Get().Positive() }
func (t *T) Other() bool      { return t.Get().Positive() }
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte(src), 0o600))
	analysis, err := analyzeThrowaway(repo, hotFunction{Path: "pkg.go", Name: "(*T).Get"})
	require.NoError(t, err)
	require.False(t, analysis.Fresh)
	require.False(t, analysis.signal())
}

// TestThrowawaySignalNeedsAtLeastTwoConsumingSites: a single consuming
// caller alongside several ordinary ones should not fire; count alone is not
// enough evidence.
func TestThrowawaySignalNeedsAtLeastTwoConsumingSites(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	src := `package pkg

type X struct{ v int }

func NewX(v int) *X { return &X{v: v} }

type T struct{ x int }

func (t *T) Get() *X { return NewX(t.x) }

func (t *T) OnlyConsumer() bool { return t.Get().v > 0 }

func (t *T) Keeps() *X   { return t.Get() }
func (t *T) Stores() *X  { v := t.Get(); return v }
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte(src), 0o600))
	analysis, err := analyzeThrowaway(repo, hotFunction{Path: "pkg.go", Name: "(*T).Get"})
	require.NoError(t, err)
	require.True(t, analysis.Fresh)
	require.Equal(t, 1, analysis.consumingCount())
	require.False(t, analysis.signal(), "one consuming call site is not enough evidence")
}

// TestAddThrowawayTargetsPrependsFirstTier exercises the causes.go wiring
// (gotorque task item 2): a throwaway_result target found on a hot function
// goes ahead of every Jev-ranked target, its remedy leads
// CandidateHypotheses, and its callers' locations are added to HotPaths so
// the excerpt collector (which is driven entirely by HotPath.Location) picks
// up their source too.
func TestAddThrowawayTargetsPrependsFirstTier(t *testing.T) {
	repo := writeThrowawayFixture(t)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get", Location: "pkg.go:14"}
	jevTarget := agents.Target{Location: "pkg.go:99", Function: "other", Cause: "fast_path", Remedy: "some other remedy"}
	result := agents.AnalystResult{
		Targets:             []agents.Target{jevTarget},
		CandidateHypotheses: []string{jevTarget.Remedy},
	}

	addThrowawayTargets(repo, []hotFunction{site}, nil, &result)

	require.Len(t, result.Targets, 2)
	require.Equal(t, causeThrowawayResult, result.Targets[0].Cause, "the code-derived target leads")
	require.Equal(t, jevTarget, result.Targets[1])
	require.Len(t, result.CandidateHypotheses, 2)
	require.Equal(t, result.Targets[0].Remedy, result.CandidateHypotheses[0])

	var sawCaller bool
	for _, hp := range result.HotPaths {
		if hp.Location != site.Location && hp.Evidence != "" {
			sawCaller = true
		}
	}
	require.True(t, sawCaller, "a consuming caller's location should reach HotPaths for the excerpt collector")
}

// manyCallersSource builds a throwaway_result fixture with n consuming
// callers, each with exactly one call site and named Caller01..CallerNN in
// zero-padded, alphabetically-ordered, source-declaration order, so a test can
// tell "ranked by weight" apart from "ranked by name" or "ranked by source
// position" -- all three would otherwise agree.
func manyCallersSource(n int) string {
	var b strings.Builder
	b.WriteString("package pkg\n\ntype X struct{ v int }\n\nfunc NewX(v int) *X { return &X{v: v} }\n\ntype T struct{ x int }\n\nfunc (t *T) Get() *X { return NewX(t.x) }\n\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "func (t *T) Caller%02d() int { return t.Get().v }\n\n", i)
	}
	return b.String()
}

func writeManyCallersFixture(t *testing.T, n int) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte(manyCallersSource(n)), 0o600))
	return repo
}

// TestConsumingCallersRankedByProfileWeight is the unit fixture for ADR
// 0027's addendum: with 13 tied consuming callers (one call site each, in
// alphabetical/source order), the two the profile measured as hot -- named
// last, so alphabetical and positional order would rank them out of an
// 11-slot cap -- come first, and both survive the cap that would otherwise
// have dropped them.
func TestConsumingCallersRankedByProfileWeight(t *testing.T) {
	repo := writeManyCallersFixture(t, 13)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get", Location: "pkg.go:9"}
	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.signal())

	weights := map[string]float64{
		"test.local/fixture.(*T).Caller12": 50,
		"test.local/fixture.(*T).Caller13": 40,
	}
	callers := analysis.consumingCallers(weights)
	require.Len(t, callers, 13)
	require.Equal(t, "(*T).Caller12", callers[0].Caller, "the hottest profiled caller ranks first")
	require.Equal(t, "(*T).Caller13", callers[1].Caller, "the second-hottest profiled caller ranks second")
	require.Equal(t, "(*T).Caller01", callers[2].Caller, "unprofiled callers keep their tie-break order after the profiled ones")

	target := throwawayTarget(site, analysis, weights)
	names := make([]string, 0, len(target.Callers))
	for _, f := range target.Callers {
		names = append(names, f.Name)
	}
	require.Contains(t, names, "(*T).Caller12", "the cap must keep a hot caller instead of dropping it for one merely earlier in the source")
	require.Contains(t, names, "(*T).Caller13")
	require.NotContains(t, names, "(*T).Caller10", "the cap has to drop something now that two unranked slots moved to the front")
	require.NotContains(t, names, "(*T).Caller11")
	require.Len(t, target.Callers, maxThrowawayFunctions-1)
}

// TestConsumingCallersTieBreakIsStableAndDeterministic: callers tied on both
// profile weight (absent from the profile, or equal within it) and call-site
// count sort by name, not by map or slice iteration order, so the same input
// always produces the same output.
func TestConsumingCallersTieBreakIsStableAndDeterministic(t *testing.T) {
	repo := writeManyCallersFixture(t, 5)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get", Location: "pkg.go:9"}
	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)

	// Every caller ties: none is in the weight map (equal, both absent), and
	// each has exactly one call site (equal counts).
	weights := map[string]float64{"unrelated.Function": 99}
	want := []string{"(*T).Caller01", "(*T).Caller02", "(*T).Caller03", "(*T).Caller04", "(*T).Caller05"}
	for i := 0; i < 5; i++ {
		callers := analysis.consumingCallers(weights)
		names := make([]string, 0, len(callers))
		for _, c := range callers {
			names = append(names, c.Caller)
		}
		require.Equal(t, want, names, "tie-break must be deterministic across repeated calls")
	}
}

// TestConsumingCallersFallsBackToCallSiteCountWithoutProfileData: a nil or
// empty weights map (a benchmark-only or otherwise empty discovery) must
// reproduce the pre-ranking order exactly -- call-site count only, stable on
// source position -- not the weighted algorithm's name tie-break.
func TestConsumingCallersFallsBackToCallSiteCountWithoutProfileData(t *testing.T) {
	repo := writeThrowawayFixture(t)
	site := hotFunction{Path: "pkg.go", Name: "(*T).Get"}
	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)

	want := []string{"(*T).IsPositive", "(*T).Value", "(*T).Sum"}
	nilNames := callerNames(analysis.consumingCallers(nil))
	emptyNames := callerNames(analysis.consumingCallers(map[string]float64{}))
	require.Equal(t, want, nilNames, "nil weights must fall back to the exact pre-ranking order")
	require.Equal(t, want, emptyNames, "an empty weights map must fall back the same way nil does")
}

func callerNames(callers []callSite) []string {
	names := make([]string, 0, len(callers))
	for _, c := range callers {
		names = append(names, c.Caller)
	}
	return names
}

// TestAddThrowawayTargetsLeavesTheResultAloneWhenTheSignalNeverFires: a
// package with no throwaway_result callee changes nothing.
func TestAddThrowawayTargetsLeavesTheResultAloneWhenTheSignalNeverFires(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte("package pkg\n\nfunc f() int { return 1 }\n"), 0o600))
	result := agents.AnalystResult{Targets: []agents.Target{{Location: "pkg.go:3", Function: "f", Cause: "fast_path"}}}
	before := result

	addThrowawayTargets(repo, []hotFunction{{Path: "pkg.go", Name: "f", Location: "pkg.go:3"}}, nil, &result)

	require.Equal(t, before, result)
}
