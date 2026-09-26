package campaign

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/profile"
	"example.com/gotorque/internal/toolchain"
)

// daselRepo is the recorded dasel clone this replay gate was written against
// (docs/adr/0027): the caller-analysis prototype's motivating case. The test
// is skipped, not failed, when it is absent, so CI (which never has it) skips
// cleanly; the gotorque task instructions call this out explicitly.
func daselRepo(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	repo := filepath.Join(home, "projects", "gotorque-work", "dasel")
	if _, err := os.Stat(filepath.Join(repo, "model", "value.go")); err != nil {
		t.Skip("dasel clone not present at ~/projects/gotorque-work/dasel; skipping the replay gate")
	}
	return repo
}

// fix1Diff is the hand-written fix this replay gate checks the shape check
// against (verified separately: tests pass, output byte-identical,
// json-filter-map -15.6%, yaml-to-json -9.7%; see the gotorque task prompt).
func fix1Diff(t *testing.T) []byte {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	path := filepath.Join(home, "projects", "gotorque-work", "recovered-2026-09-25", "dasel-audit", "fix1-unpack-noalloc.diff")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestThrowawaySignalFiresOnDaselUnpackKinds is the replay gate's live case
// (gotorque task item 5b): the signal must fire on (*Value).UnpackKinds, the
// function profiling ranked as dasel's hottest, reading the real clone
// read-only.
func TestThrowawaySignalFiresOnDaselUnpackKinds(t *testing.T) {
	repo := daselRepo(t)
	site := hotFunction{Path: "model/value.go", Name: "(*Value).UnpackKinds", Location: "model/value.go:187"}

	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.Fresh, "UnpackKinds always returns NewValue(res), a fresh allocation")
	require.True(t, analysis.signal(), "most of UnpackKinds' package-internal callers only read the result")

	// fix1 rewrites the callee plus these eleven callers (isFloat and isSlice
	// are also deleted by the same diff, but are helpers the fixed callers no
	// longer call, not callers of UnpackKinds themselves, so they are not
	// expected in the caller set -- see the ADR's note on the deletion).
	edited := []string{
		"(*Value).IsString", "(*Value).IsInt", "(*Value).IsFloat", "(*Value).IsBool",
		"(*Value).isStandardMap", "(*Value).isDencodingMap", "(*Value).IsSlice",
		"(*Value).Append", "(*Value).SliceLen", "(*Value).GetSliceIndex", "(*Value).SetSliceIndex",
	}
	found := map[string]bool{}
	for _, c := range analysis.consumingCallers(nil) {
		found[c.Caller] = true
	}
	var missing []string
	for _, name := range edited {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	require.Empty(t, missing, "the caller set must cover every function fix1 edits; found %v", found)
}

// TestThrowawayCallerRankingOnDaselRealProfile is the replay gate's fourth
// assertion (ADR 0027's addendum on caller ranking): with a real profile,
// (*Value).Append and (*Value).isDencodingMap -- both callers fix1 edits --
// outrank every caller the profile never measured, instead of losing to
// whatever call-site count or source position happened to give them.
//
// testdata/dasel-sample-profile.txt was captured with the platform sampler
// this engine actually uses (/usr/bin/sample) against a real dasel binary
// built from this same clone, running `dasel query --unstable -o json
// 'data.filter(active==true).map(name)...'` over a 1.2M-element synthetic
// JSON array for 4 seconds -- the same shape of workload discovery's own
// sampleTargetProfile runs, just captured once and committed so this test
// needs no live process. It does not reach every one of fix1's eleven
// callers (this query never touches the int, float, bool or slice-index
// accessors), which is expected and recorded rather than worked around: the
// two it does reach are exactly the evidence this test checks.
func TestThrowawayCallerRankingOnDaselRealProfile(t *testing.T) {
	repo := daselRepo(t)
	site := hotFunction{Path: "model/value.go", Name: "(*Value).UnpackKinds", Location: "model/value.go:187"}

	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.signal())

	report, err := os.ReadFile(filepath.Join("testdata", "dasel-sample-profile.txt"))
	require.NoError(t, err)
	stacks := profile.MacOSSampleStacks(string(report))
	require.NotEmpty(t, stacks, "the fixture must actually parse into stacks, or this test proves nothing")
	own := func(s string) bool { return strings.HasPrefix(s, "github.com/tomwright/dasel/v3") }
	weights := hotFunctionWeights(profile.AttributeToOwn(stacks, own))
	require.NotEmpty(t, weights)

	ranked := analysis.consumingCallers(weights)
	rank := make(map[string]int, len(ranked))
	for i, c := range ranked {
		rank[c.Caller] = i
	}
	appendRank, ok := rank["(*Value).Append"]
	require.True(t, ok, "Append must be in the consuming caller set")
	mapRank, ok := rank["(*Value).isDencodingMap"]
	require.True(t, ok, "isDencodingMap must be in the consuming caller set")

	// Every caller this real profile never measured -- most of the ~18-strong
	// caller set -- must rank behind both profiled ones.
	for _, c := range ranked {
		if c.Caller == "(*Value).Append" || c.Caller == "(*Value).isDencodingMap" {
			continue
		}
		if _, profiled := callerWeight(weights, c.Caller); profiled {
			continue
		}
		require.Greater(t, rank[c.Caller], appendRank, "%s was never profiled and must rank behind Append", c.Caller)
		require.Greater(t, rank[c.Caller], mapRank, "%s was never profiled and must rank behind isDencodingMap", c.Caller)
	}
}

// TestShapeCheckAcceptsFix1UnderTheFullCallerSet is the replay gate's third
// assertion: the shape check accepts fix1-unpack-noalloc.diff when the
// target's function set is the throwaway_result caller set found on the real
// clone. It builds a scratch git repository from the dasel model package
// (never the checked-out clone itself, which this test only ever reads),
// applies fix1 with plain `git apply`, and runs checkShape exactly as the
// engine does.
//
// The set passed here is every consuming/discarded caller analyzeThrowaway
// found, not the capped production target (maxThrowawayFunctions, ADR 0027):
// fix1 alone touches eleven callers, above the cap this ADR settled on for a
// real campaign's prompt budget. The gate is deliberately about the
// uncapped signal's coverage, and the ADR documents the resulting risk: a
// capped campaign target would not fully cover fix1's edits, and a patch
// that rewrote a caller outside the capped set would be rejected by this
// same check.
func TestShapeCheckAcceptsFix1UnderTheFullCallerSet(t *testing.T) {
	repo := daselRepo(t)
	site := hotFunction{Path: "model/value.go", Name: "(*Value).UnpackKinds", Location: "model/value.go:187"}

	analysis, err := analyzeThrowaway(repo, site)
	require.NoError(t, err)
	require.True(t, analysis.signal())

	fns := make([]agents.FunctionRef, 0, len(analysis.consumingCallers(nil)))
	for _, c := range analysis.consumingCallers(nil) {
		fns = append(fns, agents.FunctionRef{Name: c.Caller, Location: c.Location})
	}
	target := agents.Target{
		Location: site.Location,
		Function: site.Name,
		Cause:    causeThrowawayResult,
		Kind:     agents.TargetFunctionSet,
		Callers:  fns,
	}

	worktree := scratchDaselModel(t, repo)
	require.NoError(t, gitApplyDiff(t, worktree, fix1Diff(t)))
	diff, err := toolchain.New(toolchain.Options{}).ChangedLines(context.Background(), worktree)
	require.NoError(t, err)
	require.NoError(t, checkShape(worktree, diff, &target))
}

// scratchDaselModel copies dasel's model package (and a bare go.mod, so the
// files parse as their own module root, though checkShape never builds
// anything) into a fresh git repository under /private/tmp/multifn-proto-work,
// committed at the pre-fix1 state, so fix1 can be applied and diffed there
// without ever writing to the dasel clone this test reads.
func scratchDaselModel(t *testing.T, daselRepo string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/tomwright/dasel/v3\n\ngo 1.25\n"), 0o600))
	modelDir := filepath.Join(root, "model")
	require.NoError(t, os.Mkdir(modelDir, 0o750))
	entries, err := os.ReadDir(filepath.Join(daselRepo, "model"))
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		copyFile(t, filepath.Join(daselRepo, "model", e.Name()), filepath.Join(modelDir, e.Name()))
	}
	git(t, root, "init")
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return root
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	require.NoError(t, err)
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	require.NoError(t, err)
}

// gitApplyDiff runs `git apply` in worktree; the diff was recorded at dasel's
// repository root, so it applies cleanly to this scratch copy, which mirrors
// that layout (model/ at its own root).
func gitApplyDiff(t *testing.T, worktree string, diff []byte) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fix1.diff")
	require.NoError(t, os.WriteFile(path, diff, 0o600))
	cmd := exec.CommandContext(context.Background(), "git", "apply", path)
	cmd.Dir = worktree
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply: %w: %s", err, output)
	}
	return nil
}
