package campaign

import (
	"os"
	"path/filepath"
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

	callers := analysis.consumingCallers()
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

	target := throwawayTarget(site, analysis)
	require.Equal(t, causeThrowawayResult, target.Cause)
	require.Equal(t, "(*T).Get", target.Function)
	require.Equal(t, "pkg.go:14", target.Location)
	require.Contains(t, target.Remedy, "(*T).Get")

	fns := agents.DecodeFunctionSet(target.Functions)
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

	addThrowawayTargets(repo, []hotFunction{site}, &result)

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

// TestAddThrowawayTargetsLeavesTheResultAloneWhenTheSignalNeverFires: a
// package with no throwaway_result callee changes nothing.
func TestAddThrowawayTargetsLeavesTheResultAloneWhenTheSignalNeverFires(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg.go"), []byte("package pkg\n\nfunc f() int { return 1 }\n"), 0o600))
	result := agents.AnalystResult{Targets: []agents.Target{{Location: "pkg.go:3", Function: "f", Cause: "fast_path"}}}
	before := result

	addThrowawayTargets(repo, []hotFunction{{Path: "pkg.go", Name: "f", Location: "pkg.go:3"}}, &result)

	require.Equal(t, before, result)
}
