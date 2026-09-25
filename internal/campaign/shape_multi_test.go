package campaign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/toolchain"
)

// shapeMultiTarget is a throwaway_result target over the fixture
// throwawayFixtureRepo builds: (*Store).Get in a.go, with (*Store).IsPositive
// in b.go as its one consuming caller in the set.
var shapeMultiTarget = agents.Target{
	Location: "a.go:9",
	Function: "(*Store).Get",
	Cause:    causeThrowawayResult,
	Functions: agents.EncodeFunctionSet([]agents.FunctionRef{
		{Name: "(*Store).IsPositive", Location: "b.go:3"},
	}),
}

// shapeOfMulti commits throwawayFixtureRepo's two files, overwrites them with
// patchedA/patchedB, and judges the result exactly as the engine does.
func shapeOfMulti(t *testing.T, patchedA, patchedB string, target *agents.Target) error {
	t.Helper()
	repo := throwawayFixtureRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte(patchedA), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte(patchedB), 0o600))
	diff, err := toolchain.New(toolchain.Options{}).ChangedLines(context.Background(), repo)
	require.NoError(t, err)
	return checkShape(repo, diff, target)
}

const fixtureA = `package fixture

type Box struct{ v int }

func NewBox(v int) *Box { return &Box{v: v} }

type Store struct{ x int }

func (s *Store) Get() *Box {
	return NewBox(s.x)
}
`

// TestMultiConfinedAcceptsEditsAcrossTheSetsFiles: the callee (a.go) and its
// one in-set caller (b.go) may both change.
func TestMultiConfinedAcceptsEditsAcrossTheSetsFiles(t *testing.T) {
	patchedA := `package fixture

type Box struct{ v int }

func NewBox(v int) *Box { return &Box{v: v} }

type Store struct{ x int }

func (s *Store) getValue() int {
	return s.x
}

func (s *Store) Get() *Box {
	return NewBox(s.getValue())
}
`
	patchedB := `package fixture

func (s *Store) IsPositive() bool {
	return s.getValue() > 0
}

func (s *Store) Double() int {
	b := s.Get()
	return b.v * 2
}
`
	require.NoError(t, shapeOfMulti(t, patchedA, patchedB, &shapeMultiTarget))
}

// TestMultiConfinedRejectsAnEditOutsideTheSet: Double is in b.go (the same
// file as the in-set caller) but is not itself in the set, so editing its
// body must still be rejected.
func TestMultiConfinedRejectsAnEditOutsideTheSet(t *testing.T) {
	patchedB := `package fixture

func (s *Store) IsPositive() bool {
	return s.Get().v > 0
}

func (s *Store) Double() int {
	return 42
}
`
	err := shapeOfMulti(t, fixtureA, patchedB, &shapeMultiTarget)
	require.ErrorContains(t, err, "the patch edits (*Store).Double")
	require.ErrorContains(t, err, "outside the throwaway_result set")
}

// TestMultiConfinedIgnoresFilesOutsideTheCalleesDirectory: a third file, in a
// different package directory, is never confined by a throwaway_result
// target, exactly as the single-function path never confines a file other
// than the target's own.
func TestMultiConfinedIgnoresFilesOutsideTheCalleesDirectory(t *testing.T) {
	repo := throwawayFixtureRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "other"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "other", "c.go"), []byte("package other\n\nfunc Unrelated() int { return 1 }\n"), 0o600))
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "add other package")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "other", "c.go"), []byte("package other\n\nfunc Unrelated() int { return 2 }\n"), 0o600))
	// The in-set caller changes too, so the remedy rule is satisfied and only
	// confinement is under test.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte("package fixture\n\nfunc (s *Store) IsPositive() bool {\n\treturn s.x > 0\n}\n\nfunc (s *Store) Double() int {\n\tb := s.Get()\n\treturn b.v * 2\n}\n"), 0o600))
	diff, err := toolchain.New(toolchain.Options{}).ChangedLines(context.Background(), repo)
	require.NoError(t, err)
	require.NoError(t, checkShape(repo, diff, &shapeMultiTarget))
}

// TestThrowawayRemedyNeedsASwitchedCaller: the first live dasel campaign's
// candidate rewrote only the callee's internals, so the callers kept
// dropping a fresh allocation and it measured 0%. A patch that edits no
// caller in the set has not applied the remedy and is rejected before build.
func TestThrowawayRemedyNeedsASwitchedCaller(t *testing.T) {
	calleeOnly := `package fixture

type Box struct{ v int }

func NewBox(v int) *Box { return &Box{v: v} }

type Store struct{ x int }

func (s *Store) Get() *Box {
	v := s.x
	return NewBox(v)
}
`
	unchangedB := `package fixture

func (s *Store) IsPositive() bool {
	return s.Get().v > 0
}

func (s *Store) Double() int {
	b := s.Get()
	return b.v * 2
}
`
	err := shapeOfMulti(t, calleeOnly, unchangedB, &shapeMultiTarget)
	require.ErrorContains(t, err, "switching (*Store).Get's callers")
	require.ErrorContains(t, err, "(*Store).IsPositive")
}
