package campaign

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
)

// throwawayFixtureRepo is a small two-file package mirroring the dasel shape
// this multi-file builder targets: a callee (Get) in a.go that always
// allocates a fresh *Box, and two callers in b.go that each use only its
// field.
func throwawayFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	aSrc := `package fixture

type Box struct{ v int }

func NewBox(v int) *Box { return &Box{v: v} }

type Store struct{ x int }

func (s *Store) Get() *Box {
	return NewBox(s.x)
}
`
	bSrc := `package fixture

func (s *Store) IsPositive() bool {
	return s.Get().v > 0
}

func (s *Store) Double() int {
	b := s.Get()
	return b.v * 2
}
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte(aSrc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte(bSrc), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

// TestBuildMultiFunctionSourceDiffAppliesAndBuilds is the replay gate's
// end-to-end transport test (gotorque task item 5c): several function
// sources -- a brand-new helper appended to the callee's file, the callee
// rewritten to use it, and two callers in a different file switched to it
// too -- become one multi-file diff, which applies with plain `git apply`
// and, once applied, builds.
func TestBuildMultiFunctionSourceDiffAppliesAndBuilds(t *testing.T) {
	repo := throwawayFixtureRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := agents.Target{
		Location: "a.go:9",
		Function: "(*Store).Get",
		Cause:    causeThrowawayResult,
		Kind:     agents.TargetFunctionSet,
		Callers: []agents.FunctionRef{
			{Name: "(*Store).IsPositive", Location: "b.go:3"},
			{Name: "(*Store).Double", Location: "b.go:7"},
		},
	}
	sources := []string{
		// A wholly new function: not the callee, not in Functions, and not
		// declared anywhere else in the package.
		"func (s *Store) getValue() int {\n\treturn s.x\n}",
		// The callee, rewritten to use it.
		"func (s *Store) Get() *Box {\n\treturn NewBox(s.getValue())\n}",
		// The two consuming callers, rewritten to skip the allocation.
		"func (s *Store) IsPositive() bool {\n\treturn s.getValue() > 0\n}",
		"func (s *Store) Double() int {\n\treturn s.getValue() * 2\n}",
	}

	diff, err := engine.buildMultiFunctionSourceDiff(context.Background(), target, sources, nil)
	require.NoError(t, err)
	require.Contains(t, diff, "--- a/a.go")
	require.Contains(t, diff, "+++ b/a.go")
	require.Contains(t, diff, "--- a/b.go")
	require.Contains(t, diff, "+++ b/b.go")

	patchPath := filepath.Join(t.TempDir(), "candidate.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte(diff), 0o600))
	cmd := exec.CommandContext(t.Context(), "git", "apply", patchPath)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git apply: %s\ndiff:\n%s", out, diff)

	aContent, err := os.ReadFile(filepath.Join(repo, "a.go"))
	require.NoError(t, err)
	require.Contains(t, string(aContent), "func (s *Store) getValue() int")
	require.Contains(t, string(aContent), "NewBox(s.getValue())")
	bContent, err := os.ReadFile(filepath.Join(repo, "b.go"))
	require.NoError(t, err)
	require.Contains(t, string(bContent), "s.getValue() > 0")
	require.Contains(t, string(bContent), "s.getValue() * 2")

	buildCmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	buildCmd.Dir = repo
	buildOut, err := buildCmd.CombinedOutput()
	require.NoErrorf(t, err, "go build: %s", buildOut)
}

// TestBuildMultiFunctionSourceDiffRejectsANameOutsideTheSet: a
// function_sources declaration that names a function which exists elsewhere
// in the package, but is not in the target's set, is a rejection rather than
// a silent rewrite of a function the shape check never agreed to.
func TestBuildMultiFunctionSourceDiffRejectsANameOutsideTheSet(t *testing.T) {
	repo := throwawayFixtureRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := agents.Target{
		Location: "a.go:9",
		Function: "(*Store).Get",
		Cause:    causeThrowawayResult,
		Kind:     agents.TargetFunctionSet,
		Callers: []agents.FunctionRef{
			{Name: "(*Store).IsPositive", Location: "b.go:3"},
		},
	}
	sources := []string{
		"func (s *Store) Get() *Box {\n\treturn NewBox(s.x)\n}",
		// Double exists in the package but is outside the set.
		"func (s *Store) Double() int {\n\treturn 0\n}",
	}
	_, err := engine.buildMultiFunctionSourceDiff(context.Background(), target, sources, nil)
	require.ErrorContains(t, err, "Double")
	require.ErrorContains(t, err, "outside the throwaway_result set")
}

// throwawayImportFixtureRepo is throwawayFixtureRepo's shape, except b.go's
// caller does not yet need any import -- the tests below rewrite it to need
// strconv, in a file distinct from the callee's own (a.go).
func throwawayImportFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	aSrc := `package fixture

type Box struct{ v int }

func NewBox(v int) *Box { return &Box{v: v} }

type Store struct{ x int }

func (s *Store) Get() *Box {
	return NewBox(s.x)
}
`
	bSrc := `package fixture

func (s *Store) Describe() int {
	return s.Get().v
}
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte(aSrc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte(bSrc), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

// throwawayImportTarget and throwawayImportSources are shared by the two
// per-file import tests below: Describe (b.go) is rewritten to call
// strconv.Itoa, a package a.go's callee never needs.
func throwawayImportTarget() agents.Target {
	return agents.Target{
		Location: "a.go:9",
		Function: "(*Store).Get",
		Cause:    causeThrowawayResult,
		Kind:     agents.TargetFunctionSet,
		Callers: []agents.FunctionRef{
			{Name: "(*Store).Describe", Location: "b.go:3"},
		},
	}
}

func throwawayImportSources() []string {
	return []string{
		"func (s *Store) Get() *Box {\n\treturn NewBox(s.x)\n}",
		"func (s *Store) Describe() int {\n\treturn len(strconv.Itoa(s.Get().v))\n}",
	}
}

// TestMultiFunctionSourceAddsImportToTheFileThatNeedsIt: a caller in a file
// other than the callee's newly uses strconv, listed in imports. The diff
// must add the import to b.go, not a.go, and the result must build.
func TestMultiFunctionSourceAddsImportToTheFileThatNeedsIt(t *testing.T) {
	repo := throwawayImportFixtureRepo(t)
	engine := newFuncSourceTestEngine(repo)

	diff, err := engine.buildMultiFunctionSourceDiff(context.Background(), throwawayImportTarget(), throwawayImportSources(), []string{"strconv"})
	require.NoError(t, err)

	patchPath := filepath.Join(t.TempDir(), "candidate.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte(diff), 0o600))
	cmd := exec.CommandContext(t.Context(), "git", "apply", patchPath)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git apply: %s\ndiff:\n%s", out, diff)

	aContent, err := os.ReadFile(filepath.Join(repo, "a.go"))
	require.NoError(t, err)
	require.NotContains(t, string(aContent), `"strconv"`)

	bContent, err := os.ReadFile(filepath.Join(repo, "b.go"))
	require.NoError(t, err)
	require.Contains(t, string(bContent), `"strconv"`)

	buildCmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	buildCmd.Dir = repo
	buildOut, err := buildCmd.CombinedOutput()
	require.NoErrorf(t, err, "go build: %s", buildOut)
}

// TestMultiFunctionSourceInfersStdlibImportWithNoImportsListed: the same
// rewrite, but the optimizer sent no imports at all. The deterministic
// standard-library inference (stdlibImportPath) must still add strconv to
// b.go so the candidate builds.
func TestMultiFunctionSourceInfersStdlibImportWithNoImportsListed(t *testing.T) {
	repo := throwawayImportFixtureRepo(t)
	engine := newFuncSourceTestEngine(repo)

	diff, err := engine.buildMultiFunctionSourceDiff(context.Background(), throwawayImportTarget(), throwawayImportSources(), nil)
	require.NoError(t, err)

	patchPath := filepath.Join(t.TempDir(), "candidate.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte(diff), 0o600))
	cmd := exec.CommandContext(t.Context(), "git", "apply", patchPath)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git apply: %s\ndiff:\n%s", out, diff)

	buildCmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	buildCmd.Dir = repo
	buildOut, err := buildCmd.CombinedOutput()
	require.NoErrorf(t, err, "go build: %s", buildOut)
}

// TestMultiFunctionSourceNeverAddsAnUnusedImport: an import listed but
// referenced by no touched file's new code is added nowhere.
func TestMultiFunctionSourceNeverAddsAnUnusedImport(t *testing.T) {
	repo := throwawayFixtureRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := agents.Target{
		Location: "a.go:9",
		Function: "(*Store).Get",
		Cause:    causeThrowawayResult,
		Kind:     agents.TargetFunctionSet,
		Callers: []agents.FunctionRef{
			{Name: "(*Store).IsPositive", Location: "b.go:3"},
			{Name: "(*Store).Double", Location: "b.go:7"},
		},
	}
	sources := []string{
		"func (s *Store) Get() *Box {\n\treturn NewBox(s.x)\n}",
		"func (s *Store) IsPositive() bool {\n\treturn s.Get().v > 0\n}",
		"func (s *Store) Double() int {\n\treturn s.Get().v * 2\n}",
	}

	diff, err := engine.buildMultiFunctionSourceDiff(context.Background(), target, sources, []string{"strconv"})
	require.NoError(t, err)
	require.NotContains(t, diff, "strconv")
}
