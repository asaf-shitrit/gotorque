package campaign

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/toolchain"
)

// newFuncSourceTestEngine builds a bare *Engine pointed at repo with a real
// toolchain, for tests exercising buildFunctionSourceDiff /
// resolveCandidatePatch directly without a full Create campaign.
func newFuncSourceTestEngine(repo string) *Engine {
	e := &Engine{toolchain: toolchain.New(toolchain.Options{})}
	e.state.Repository = repo
	return e
}

// applyDiff applies diff to repo with git apply and returns the resulting
// content of path.
func applyDiff(t *testing.T, repo, diff, path string) string {
	t.Helper()
	patchPath := filepath.Join(t.TempDir(), "candidate.diff")
	require.NoError(t, os.WriteFile(patchPath, []byte(diff), 0o600))
	cmd := exec.CommandContext(t.Context(), "git", "apply", "--unidiff-zero", patchPath)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git apply: %s\ndiff:\n%s", out, diff)
	content, err := os.ReadFile(filepath.Join(repo, path))
	require.NoError(t, err)
	return string(content)
}

func methodRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	src := `package main

import "os"

type cli struct{ out *os.File }

// printValues writes every value on its own line.
func (c *cli) printValues(vs []string) error {
	for _, v := range vs {
		c.out.WriteString(v)
		c.out.WriteString("\n")
	}
	return nil
}

func other() int { return 1 }
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(src), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

func TestReplaceFunctionSourceMethodKeepsOldDocWhenNewHasNone(t *testing.T) {
	repo := methodRepo(t)
	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	newSrc := `func (c *cli) printValues(vs []string) error {
	w := bufio.NewWriter(c.out)
	defer w.Flush()
	for _, v := range vs {
		w.WriteString(v)
		w.WriteString("\n")
	}
	return nil
}`
	updated, err := replaceFunctionSource(filepath.Join(repo, "main.go"), original, "(*cli).printValues", newSrc, []string{"bufio"})
	require.NoError(t, err)
	require.Contains(t, string(updated), "// printValues writes every value on its own line.", "old doc comment kept when new source carries none")
	require.Contains(t, string(updated), "bufio.NewWriter")
	require.Contains(t, string(updated), "\"bufio\"")
	require.Contains(t, string(updated), "\"os\"")
}

func TestReplaceFunctionSourcePlainFunctionAndNewDocReplacesOld(t *testing.T) {
	repo := t.TempDir()
	src := `package main

// old doc, to be replaced.
func add(a, b int) int {
	return a + b
}
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	path := filepath.Join(repo, "main.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")

	newSrc := `// add sums a and b.
func add(a, b int) int {
	return a + b + 0
}`
	updated, err := replaceFunctionSource(path, []byte(src), "add", newSrc, nil)
	require.NoError(t, err)
	require.Contains(t, string(updated), "// add sums a and b.")
	require.NotContains(t, string(updated), "old doc, to be replaced.")
}

func TestReplaceFunctionSourceRejectsWrongName(t *testing.T) {
	repo := methodRepo(t)
	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	_, err = replaceFunctionSource(filepath.Join(repo, "main.go"), original, "(*cli).printValues", "func other2() int { return 2 }", nil)
	require.ErrorContains(t, err, "not the target")
}

func TestReplaceFunctionSourceRejectsUnparseableSource(t *testing.T) {
	repo := methodRepo(t)
	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	_, err = replaceFunctionSource(filepath.Join(repo, "main.go"), original, "(*cli).printValues", "func broken( {", nil)
	require.ErrorContains(t, err, "does not parse")
}

// TestReplaceFunctionSourceInsertsAFreshImportBlock covers the file-has-no-
// imports-yet path of addImports (insertImportBlock).
func TestReplaceFunctionSourceInsertsAFreshImportBlock(t *testing.T) {
	repo := t.TempDir()
	src := "package main\n\nfunc f() int {\n\treturn 1\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	path := filepath.Join(repo, "main.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")

	newSrc := `func f() int {
	var b strings.Builder
	b.WriteString("x")
	return b.Len()
}`
	updated, err := replaceFunctionSource(path, []byte(src), "f", newSrc, []string{"strings"})
	require.NoError(t, err)
	require.Contains(t, string(updated), "import (\n\t\"strings\"\n)")
	require.Contains(t, string(updated), "strings.Builder")
}

func TestReplaceFunctionSourceRejectsMultipleDecls(t *testing.T) {
	repo := methodRepo(t)
	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	_, err = replaceFunctionSource(filepath.Join(repo, "main.go"), original, "(*cli).printValues", "func a() {}\nfunc b() {}", nil)
	require.ErrorContains(t, err, "exactly one function")
}

// TestBuildFunctionSourceDiffAppliesCleanly drives the whole builder: read
// the base file at the canonical repository, splice, add an import, diff,
// and check the produced unified diff applies with plain `git apply` and
// yields exactly the expected file content.
func TestBuildFunctionSourceDiffAppliesCleanly(t *testing.T) {
	repo := methodRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := agents.Target{Location: "main.go:8", Function: "(*cli).printValues"}
	newSrc := `func (c *cli) printValues(vs []string) error {
	w := bufio.NewWriter(c.out)
	defer w.Flush()
	for _, v := range vs {
		w.WriteString(v)
		w.WriteString("\n")
	}
	return nil
}`
	diff, err := engine.buildFunctionSourceDiff(context.Background(), target, newSrc, []string{"bufio"})
	require.NoError(t, err)
	require.Contains(t, diff, "--- a/main.go")
	require.Contains(t, diff, "+++ b/main.go")

	got := applyDiff(t, repo, diff, "main.go")
	require.Contains(t, got, "bufio.NewWriter")
	require.Contains(t, got, "\"bufio\"")
	require.Contains(t, got, "// printValues writes every value on its own line.")
}

func TestResolveCandidatePatchPrefersPatchOverFunctionSource(t *testing.T) {
	repo := methodRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := &agents.Target{Location: "main.go:8", Function: "(*cli).printValues"}
	req := orchestrator.CandidateRequest{
		Target:   target,
		Proposal: agents.OptimizerResult{Patch: "hand written diff", FunctionSource: "func (c *cli) printValues(vs []string) error { return nil }"},
	}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "hand written diff", patch)
	require.Equal(t, PatchTransport, transport)
}

func TestResolveCandidatePatchWithoutTargetLeavesPatchAlone(t *testing.T) {
	engine := &Engine{}
	req := orchestrator.CandidateRequest{
		Proposal: agents.OptimizerResult{FunctionSource: "func f() {}"},
	}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, patch, "no target means function_source is never used, matching the pre-ADR-0022 behavior")
	require.Equal(t, PatchTransport, transport)
}

func TestResolveCandidatePatchBuildsFromFunctionSourceWithTarget(t *testing.T) {
	repo := methodRepo(t)
	engine := newFuncSourceTestEngine(repo)

	target := &agents.Target{Location: "main.go:8", Function: "(*cli).printValues"}
	req := orchestrator.CandidateRequest{
		Target: target,
		Proposal: agents.OptimizerResult{FunctionSource: `func (c *cli) printValues(vs []string) error {
	w := bufio.NewWriter(c.out)
	defer w.Flush()
	for _, v := range vs {
		w.WriteString(v)
	}
	return nil
}`, Imports: []string{"bufio"}},
	}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, patch)
	require.Equal(t, FunctionSourceTransport, transport)
}

// TestEvaluateCandidateBuildsFromFunctionSource is an engine-level test: a
// proposal that carries function_source and a target reaches build with a
// correctly assembled patch, and the candidate record names the transport.
func TestEvaluateCandidateBuildsFromFunctionSource(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	newSrc := `func main() {
	b, _ := os.ReadFile("fixture.txt")
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintf(w, "%s", b)
}`
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Target:   target,
		Proposal: agents.OptimizerResult{Hypothesis: "buffer stdout", FunctionSource: newSrc, Imports: []string{"bufio"}},
	})
	require.NoError(t, err)
	require.Equal(t, FunctionSourceTransport, evidence.Candidate.Transport)
	require.False(t, evidence.Unmeasured, "evidence: %s / %s", evidence.Summary, evidence.FailureDetail)
	require.NotEmpty(t, evidence.Candidate.PatchPath)
	data, err := os.ReadFile(evidence.Candidate.PatchPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "bufio.NewWriter")
}
