package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/toolchain"
)

// printerSource is the shape of gojq's printValues: a method that writes each
// record to an unbuffered file, next to a function no patch should touch.
const printerSource = `package main

import "os"

type cli struct{ out *os.File }

func (c *cli) printValues(vs []string) error {
	for _, v := range vs {
		c.out.WriteString(v)
		c.out.WriteString("\n")
	}
	return nil
}

func other() int { return 1 }

func main() { _ = (&cli{out: os.Stdout}).printValues([]string{"a"}) }
`

var printerTarget = agents.Target{Location: "main.go:8", Function: "(*cli).printValues", Cause: "unbuffered_io", Remedy: "Route the writes through bufio."}

// buffered is the fix gojq's optimizer wrote, with or without the import.
func buffered(withImport bool) string {
	src := strings.Replace(printerSource, "\tfor _, v := range vs {\n\t\tc.out.WriteString(v)\n\t\tc.out.WriteString(\"\\n\")\n\t}",
		"\tw := bufio.NewWriter(c.out)\n\tdefer w.Flush()\n\tfor _, v := range vs {\n\t\tw.WriteString(v)\n\t\tw.WriteString(\"\\n\")\n\t}", 1)
	if withImport {
		src = strings.Replace(src, `import "os"`, "import (\n\t\"bufio\"\n\t\"os\"\n)", 1)
	}
	return src
}

// shapeOf commits printerSource, overwrites main.go with patched, and judges
// the change exactly as the engine does: Git's zero-context diff of the
// worktree, then checkShape.
func shapeOf(t *testing.T, patched string, target *agents.Target) error {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(printerSource), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(patched), 0o600))
	diff, err := toolchain.New(toolchain.Options{}).ChangedLines(context.Background(), repo)
	require.NoError(t, err)
	return checkShape(repo, diff, target)
}

func TestShapeRejectsAPackageUsedWithoutItsImport(t *testing.T) {
	err := shapeOf(t, buffered(false), &printerTarget)
	require.ErrorContains(t, err, "uses bufio without importing it")
	require.NoError(t, shapeOf(t, buffered(true), &printerTarget))
}

func TestShapeTakesALocalNameForTheVariable(t *testing.T) {
	patched := strings.Replace(printerSource, "\treturn nil\n}\n\nfunc other", "\tbytes := []byte(\"x\")\n\t_ = bytes.String\n\treturn nil\n}\n\nfunc other", 1)
	patched = strings.Replace(patched, "_ = bytes.String", "_ = len(bytes)", 1)
	require.NoError(t, shapeOf(t, patched, nil))
	shadowed := strings.Replace(printerSource, "func other() int { return 1 }", "type buffer struct{ Len int }\n\nfunc other() int {\n\tvar bytes buffer\n\treturn bytes.Len\n}", 1)
	require.NoError(t, shapeOf(t, shadowed, nil))
}

func TestShapeRejectsAnEditToAnotherFunction(t *testing.T) {
	patched := strings.Replace(buffered(true), "func other() int { return 1 }", "func other() int { return 2 }", 1)
	err := shapeOf(t, patched, &printerTarget)
	require.ErrorContains(t, err, "the patch edits other, but its target is (*cli).printValues")
}

func TestShapeAllowsANewHelper(t *testing.T) {
	patched := strings.Replace(buffered(true), "func other()", "func newline(w *bufio.Writer) { w.WriteByte('\\n') }\n\nfunc other()", 1)
	require.NoError(t, shapeOf(t, patched, &printerTarget))
}

func TestShapeHoldsAPatchToItsRemedy(t *testing.T) {
	unrelated := strings.Replace(printerSource, "\t\tc.out.WriteString(\"\\n\")\n", "", 1)
	require.ErrorContains(t, shapeOf(t, unrelated, &printerTarget), "adds no bufio reader or writer")

	unflushed := strings.Replace(buffered(true), "\tdefer w.Flush()\n", "", 1)
	require.ErrorContains(t, shapeOf(t, unflushed, &printerTarget), "never flushes it")

	prealloc := printerTarget
	prealloc.Cause = "prealloc"
	require.ErrorContains(t, shapeOf(t, unrelated, &prealloc), "adds no make or Grow call")

	// A cause with many possible fixes is not held to any one of them.
	alloc := printerTarget
	alloc.Cause = "alloc"
	require.NoError(t, shapeOf(t, unrelated, &alloc))
}

func TestShapeRejectsAFileThatDoesNotParse(t *testing.T) {
	broken := strings.Replace(printerSource, "\treturn nil\n}", "\treturn nil\n", 1)
	err := shapeOf(t, broken, nil)
	require.ErrorContains(t, err, "main.go does not parse after the patch")
	require.NotContains(t, err.Error(), string(filepath.Separator)+"main.go", "the reason names the file repository-relative")
}

func TestParseChangesAnchorsADeletion(t *testing.T) {
	changes := parseChanges([]byte("diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -4 +3,0 @@\n-\tgone()\n@@ -9,2 +8,3 @@\n-a\n-b\n+c\n+d\n+e\ndiff --git a/y.txt b/y.txt\n--- a/y.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-y\n"))
	require.Len(t, changes, 1)
	x := changes["x.go"]
	require.Equal(t, map[int]bool{3: true, 8: true, 9: true, 10: true}, x.lines)
	require.Equal(t, []string{"c", "d", "e"}, x.added)
	require.Equal(t, []string{"\tgone()", "a", "b"}, x.removed)
}

// TestEvaluateCandidateRejectsAMissingImportBeforeBuild drives the engine with
// the gojq patch: it is refused before any build, marked unmeasured so its
// target stays open, and the reason names the missing import.
func TestEvaluateCandidateRejectsAMissingImportBeforeBuild(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	patch := "--- a/main.go\n+++ b/main.go\n@@ -3 +3 @@\n-func main(){ b,_:=os.ReadFile(\"fixture.txt\"); fmt.Printf(\"%s\", b) }\n+func main(){ b,_:=os.ReadFile(\"fixture.txt\"); w := bufio.NewWriter(os.Stdout); defer w.Flush(); fmt.Fprintf(w, \"%s\", b) }\n"
	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Target:   target,
		Proposal: agents.OptimizerResult{Hypothesis: "buffer stdout", Patch: patch},
	})
	require.NoError(t, err)
	require.Contains(t, evidence.Summary, "candidate rejected before build: patch shape")
	require.Contains(t, evidence.FailureDetail, "uses bufio without importing it")
	require.True(t, evidence.Unmeasured)
	require.Len(t, evidence.ArtifactURIs, 1, "nothing was built")
}
