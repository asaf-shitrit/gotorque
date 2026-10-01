package campaign

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
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

// TestReplaceFunctionSourceInfersAStdlibImport pins the single-function
// path's standard-library inference: a replacement that uses strconv and
// strings with no imports listed still gets both, and an import the file
// already binds under an alias is not added twice.
func TestReplaceFunctionSourceInfersAStdlibImport(t *testing.T) {
	src := "package main\n\nimport str \"strings\"\n\nvar _ = str.ToUpper\n\nfunc f(n int) string {\n\treturn \"\"\n}\n"
	path := filepath.Join(t.TempDir(), "main.go")
	newSrc := `func f(n int) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(n))
	return str.ToLower(b.String())
}`
	updated, err := replaceFunctionSource(path, []byte(src), "f", newSrc, nil)
	require.NoError(t, err)
	require.Contains(t, string(updated), "\"strconv\"")
	require.Contains(t, string(updated), "str \"strings\"")
	require.Equal(t, 1, strings.Count(string(updated), "\"strings\""), "the aliased import already provides the path")
}

func TestInferredStdlibImportsLeavesUnknownNamesAlone(t *testing.T) {
	src := []byte("package main\n\nimport \"bytes\"\n\nfunc f() { _ = bytes.NewBuffer; _ = yaml.Marshal; _ = sort.Ints }\n")
	got, err := inferredStdlibImports("main.go", src)
	require.NoError(t, err)
	require.Equal(t, []string{"sort"}, got)

	_, err = inferredStdlibImports("main.go", []byte("package main\nfunc {"))
	require.ErrorContains(t, err, "does not parse")
}

// TestReplaceFunctionSourceRejectsDeclsWithoutTheTarget: several
// declarations are allowed only when one of them is the target (the rest
// are new helpers, see TestReplaceFunctionSourceAddsNewHelpers).
func TestReplaceFunctionSourceRejectsDeclsWithoutTheTarget(t *testing.T) {
	repo := methodRepo(t)
	original, err := os.ReadFile(filepath.Join(repo, "main.go"))
	require.NoError(t, err)
	_, err = replaceFunctionSource(filepath.Join(repo, "main.go"), original, "(*cli).printValues", "func a() {}\nfunc b() {}", nil)
	require.ErrorContains(t, err, "not the target (*cli).printValues")
	_, _, err = formatFunctionDecl("func a() {}\nfunc b() {}")
	require.ErrorContains(t, err, "exactly one function", "the free-choice path still takes one declaration")
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
		Proposal: agents.OptimizerResult{Patch: handDiff, FunctionSource: "func (c *cli) printValues(vs []string) error { return nil }"},
	}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, handDiff, patch)
	require.Equal(t, PatchTransport, transport)
}

const handDiff = "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-x\n+y\n"

// TestResolveCandidatePatchSkipsAPatchFieldWithNoDiff: live cue candidates
// arrived with a patch of nothing but a newline, which used to win over the
// function_source and be rejected as a malformed diff. A patch field with no
// hunk now yields to a function_source, and is still used, and judged, when
// no function_source came with it.
func TestResolveCandidatePatchSkipsAPatchFieldWithNoDiff(t *testing.T) {
	repo := methodRepo(t)
	engine := newFuncSourceTestEngine(repo)
	target := &agents.Target{Location: "main.go:8", Function: "(*cli).printValues"}
	src := "func (c *cli) printValues(vs []string) error { return nil }"
	for _, blank := range []string{"\n", "  ", "see function_source"} {
		req := orchestrator.CandidateRequest{Target: target, Proposal: agents.OptimizerResult{Patch: blank, FunctionSource: src}}
		patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, FunctionSourceTransport, transport, "patch %q", blank)
		require.Contains(t, patch, "@@")
	}
	req := orchestrator.CandidateRequest{Target: target, Proposal: agents.OptimizerResult{Patch: "see function_source"}}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "see function_source", patch)
	require.Equal(t, PatchTransport, transport)
}

// TestResolveCandidatePatchLocatesAnUntargetedFunctionSource covers free
// choice: with no target, a function_source is found by the function it
// declares and built into a diff, instead of being dropped as an empty patch.
func TestResolveCandidatePatchLocatesAnUntargetedFunctionSource(t *testing.T) {
	repo := methodRepo(t)
	engine := newFuncSourceTestEngine(repo)
	req := orchestrator.CandidateRequest{Proposal: agents.OptimizerResult{FunctionSource: `func (c *cli) printValues(vs []string) error {
	w := bufio.NewWriter(c.out)
	defer w.Flush()
	for _, v := range vs {
		w.WriteString(v)
	}
	return nil
}`, Imports: []string{"bufio"}}}
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, FunctionSourceTransport, transport)
	require.Contains(t, applyDiff(t, repo, patch, "main.go"), "bufio.NewWriter")
}

func TestResolveCandidatePatchRejectsAnUnknownUntargetedFunction(t *testing.T) {
	engine := newFuncSourceTestEngine(methodRepo(t))
	req := orchestrator.CandidateRequest{Proposal: agents.OptimizerResult{FunctionSource: "func nowhere() {}"}}
	_, transport, err := engine.resolveCandidatePatch(context.Background(), req)
	require.ErrorContains(t, err, "no non-test file declares")
	require.Equal(t, FunctionSourceTransport, transport)

	_, _, err = engine.resolveCandidatePatch(context.Background(), orchestrator.CandidateRequest{Proposal: agents.OptimizerResult{FunctionSource: "not go"}})
	require.ErrorContains(t, err, "does not parse")
}

func TestResolveCandidatePatchNamesAnEmptyProposal(t *testing.T) {
	engine := &Engine{}
	_, _, err := engine.resolveCandidatePatch(context.Background(), orchestrator.CandidateRequest{Proposal: agents.OptimizerResult{Hypothesis: "h"}})
	require.ErrorIs(t, err, errEmptyProposal)
	target := &agents.Target{Location: "main.go:8", Function: "f"}
	_, _, err = engine.resolveCandidatePatch(context.Background(), orchestrator.CandidateRequest{Target: target, Proposal: agents.OptimizerResult{Hypothesis: "h"}})
	require.ErrorIs(t, err, errEmptyProposal)
}

func TestLocateFunctionSkipsTestsVendorAndTestdataAndRefusesAmbiguity(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(src), 0o600))
	}
	write("a/a.go", "package a\n\nfunc only() {}\n\nfunc twice() {}\n")
	write("b/b.go", "package b\n\nfunc twice() {}\n")
	write("a/a_test.go", "package a\n\nfunc only() {}\n")
	write("vendor/v/v.go", "package v\n\nfunc only() {}\n")
	write("a/testdata/t.go", "package t\n\nfunc only() {}\n")
	write(".hidden/h.go", "package h\n\nfunc only() {}\n")
	write("broken.go", "package main\nfunc {")

	loc, err := locateFunction(repo, "only", nil)
	require.NoError(t, err)
	require.Equal(t, "a/a.go:3", loc)

	_, err = locateFunction(repo, "twice", nil)
	require.ErrorContains(t, err, "2 files declare")

	loc, err = locateFunction(repo, "twice", []string{"b/b.go:3"})
	require.NoError(t, err)
	require.Equal(t, "b/b.go:3", loc, "the hot file breaks the tie")

	_, err = locateFunction(repo, "twice", []string{"c/c.go:1"})
	require.ErrorContains(t, err, "2 files declare", "a hot list naming neither leaves the tie")
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

// TestAddedImportsKeepTheFileGroups: gojq's cli.go keeps the standard library,
// a third-party package and its own module in three groups. Adding bufio must
// put it in the first group and leave the other two exactly as they were.
func TestAddedImportsKeepTheFileGroups(t *testing.T) {
	src := "// Package cli implements the gojq command.\npackage cli\n\nimport (\n\t\"errors\"\n\t\"io\"\n\t\"os\"\n\n\t\"github.com/mattn/go-isatty\"\n\n\t\"github.com/itchyny/gojq\"\n)\n\nfunc f() {}\n"
	got, err := addImports("cli.go", []byte(src), []string{"bufio", "os"})
	require.NoError(t, err)
	require.Equal(t, "// Package cli implements the gojq command.\npackage cli\n\nimport (\n\t\"bufio\"\n\t\"errors\"\n\t\"io\"\n\t\"os\"\n\n\t\"github.com/mattn/go-isatty\"\n\n\t\"github.com/itchyny/gojq\"\n)\n\nfunc f() {}\n", string(got))

	single, err := addImports("a.go", []byte("package a\n\nimport \"os\"\n\nfunc f() {}\n"), []string{"bufio"})
	require.NoError(t, err)
	require.Equal(t, "package a\n\nimport (\n\t\"bufio\"\n\t\"os\"\n)\n\nfunc f() {}\n", string(single))
}

// A function_source code cannot turn into a diff is a rejection, not a crash:
// evidence without a candidate ID made the orchestrator stop a live dasel
// campaign ("evaluate candidate: empty candidate ID").
func TestEvaluateCandidateRejectsAnUnbuildableFunctionSourceWithAnID(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	target := &agents.Target{Location: "main.go:3", Function: "main", Cause: "unbuffered_io"}
	evidence, err := engine.evaluateCandidate(context.Background(), orchestrator.CandidateRequest{
		Campaign: orchestrator.CampaignRequest{BaseRevision: engine.State().Environment.Revision},
		Attempt:  1,
		Target:   target,
		Proposal: agents.OptimizerResult{Hypothesis: "rename", FunctionSource: "func other() {}"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, evidence.Candidate.ID)
	require.True(t, evidence.Unmeasured)
	require.Contains(t, evidence.Summary, "candidate rejected before build")
	require.Contains(t, evidence.FailureDetail, "not the target main")
}

func TestResolveCandidatePatchNamesAnOptimizerThatDidNotAnswer(t *testing.T) {
	engine := &Engine{}
	req := orchestrator.CandidateRequest{RoleFailure: "optimizer model call failed after 3 attempts: model call attempt exceeded its 6m0s budget"}
	_, _, err := engine.resolveCandidatePatch(context.Background(), req)
	require.ErrorContains(t, err, "the optimizer did not answer: optimizer model call failed after 3 attempts")
	require.NotErrorIs(t, err, errEmptyProposal)
}

func TestCandidatePatchKeepsTheOptimizerAnswerBesideIt(t *testing.T) {
	engine := &Engine{dir: t.TempDir()}
	req := orchestrator.CandidateRequest{Attempt: 3, Proposal: agents.OptimizerResult{Patch: "\n", FunctionSource: "func f() {}", Hypothesis: "h"}}
	id, patchPath, err := engine.writeCandidatePatch(req, "\n")
	require.NoError(t, err)
	require.FileExists(t, patchPath)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(patchPath), id+".proposal.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"function_source": "func f() {}"`)
}

// TestResolveCandidatePatchNamesTheTargetFileForAHeaderlessHunk: a live cue
// answer sent a correct hunk with no file headers. With a target the file is
// known; without one, or when the patch names its file, nothing changes.
func TestResolveCandidatePatchNamesTheTargetFileForAHeaderlessHunk(t *testing.T) {
	engine := newFuncSourceTestEngine(methodRepo(t))
	target := &agents.Target{Location: "internal/core/adt/conjunct.go:39", Function: "(*nodeContext).scheduleConjunct"}
	hunk := "@@ -77,1 +77,1 @@\n-a\n+b\n"
	patch, transport, err := engine.resolveCandidatePatch(context.Background(), orchestrator.CandidateRequest{Target: target, Proposal: agents.OptimizerResult{Patch: hunk}})
	require.NoError(t, err)
	require.Equal(t, PatchTransport, transport)
	require.Equal(t, "--- a/internal/core/adt/conjunct.go\n+++ b/internal/core/adt/conjunct.go\n"+hunk, patch)

	patch, _, err = engine.resolveCandidatePatch(context.Background(), orchestrator.CandidateRequest{Target: target, Proposal: agents.OptimizerResult{Patch: handDiff}})
	require.NoError(t, err)
	require.Equal(t, handDiff, patch)

	require.Equal(t, hunk, withTargetHeaders(hunk, nil))
}

// TestReplaceFunctionSourceAddsNewHelpers: a function_source may carry new
// helper functions beside the target; they are placed after it. A helper that
// shares a name with an existing function, or a source without the target,
// is refused.
func TestReplaceFunctionSourceAddsNewHelpers(t *testing.T) {
	src := "package main\n\nfunc f(n int) int {\n\treturn n\n}\n\nfunc g() {}\n"
	path := filepath.Join(t.TempDir(), "main.go")
	withHelper := "func f(n int) int {\n\treturn double(n) / 2\n}\n\n// double doubles n.\nfunc double(n int) int { return n * 2 }"
	updated, err := replaceFunctionSource(path, []byte(src), "f", withHelper, nil)
	require.NoError(t, err)
	require.Contains(t, string(updated), "return double(n) / 2\n}\n\n// double doubles n.\nfunc double(n int) int { return n * 2 }\n\nfunc g() {}")

	_, err = replaceFunctionSource(path, []byte(src), "f", "func f(n int) int { return n }\n\nfunc g() { println() }", nil)
	require.ErrorContains(t, err, "redeclares g")
	_, err = replaceFunctionSource(path, []byte(src), "f", "func h() {}\n\nfunc k() {}", nil)
	require.ErrorContains(t, err, "not the target f")
	_, err = replaceFunctionSource(path, []byte(src), "f", "import \"fmt\"\n\nfunc f(n int) int { fmt.Println(); return n }", nil)
	require.ErrorContains(t, err, "list import paths in imports instead")
}

// TestReplaceFunctionSourceAddsNewPackageLevelNames: held-out dyff's optimizer
// hoisted a per-call map literal into a package-level var. New vars, consts
// and types are placed before the target; one that redeclares a name the file
// already has is refused.
func TestReplaceFunctionSourceAddsNewPackageLevelNames(t *testing.T) {
	src := "package main\n\nvar existing = 1\n\nfunc f(n int) int {\n\tm := map[string]int{\"a\": 1}\n\treturn m[\"a\"] + n\n}\n"
	path := filepath.Join(t.TempDir(), "main.go")
	hoisted := "var table = map[string]int{\"a\": 1}\n\ntype unit int\n\nfunc f(n int) int {\n\treturn table[\"a\"] + n + int(unit(0))\n}"
	updated, err := replaceFunctionSource(path, []byte(src), "f", hoisted, nil)
	require.NoError(t, err)
	require.Contains(t, string(updated), "var table = map[string]int{\"a\": 1}\n\ntype unit int\n\nfunc f(n int) int {\n\treturn table[\"a\"] + n + int(unit(0))\n}")

	_, err = replaceFunctionSource(path, []byte(src), "f", "var existing = 2\n\nfunc f(n int) int { return existing + n }", nil)
	require.ErrorContains(t, err, "redeclares existing")
	_, err = replaceFunctionSource(path, []byte(src), "f", "const f = 1", nil)
	require.ErrorContains(t, err, "redeclares f")
}

// TestAProposalThatNeverBecameAPatchIsKept: held-out dyff lost two attempts to
// a function_source rejection before any diff existed, and nothing recorded
// what the optimizer had sent.
func TestAProposalThatNeverBecameAPatchIsKept(t *testing.T) {
	engine := newFuncSourceTestEngine(methodRepo(t))
	engine.dir = t.TempDir()
	req := orchestrator.CandidateRequest{
		Attempt: 3,
		Target:  &agents.Target{Location: "main.go:8", Function: "(*cli).printValues"},
		Proposal: agents.OptimizerResult{
			FunctionSource: "import \"os\"\n\nfunc (c *cli) printValues(vs []string) error { return os.ErrClosed }",
			Hypothesis:     "h",
		},
	}
	evidence, err := engine.evaluateCandidate(context.Background(), req)
	require.NoError(t, err)
	require.Contains(t, evidence.Summary, "candidate rejected before build")
	data, err := os.ReadFile(filepath.Join(engine.dir, "patches", evidence.Candidate.ID+".proposal.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), "import")
}
