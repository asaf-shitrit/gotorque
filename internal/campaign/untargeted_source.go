package campaign

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

// untargetedFunctionSourceDiff builds the diff for a free-choice proposal (no
// code-chosen target) that answered with function_source instead of patch.
// The optimizer's instruction describes function_source at length, and a
// free-choice function_source used to be dropped without a trace: on
// overnight-go-jsonnet-history-1 and overnight-chroma-1, three free-choice
// proposals arrived with a hypothesis and an empty patch and were rejected as
// "patch is empty", with nothing recording whether they carried a function
// source. The declaration names its own function, so code looks that
// function up in the base revision, and with exactly one match it builds the
// diff the same way the targeted path does. The optimizer still chose the
// site; code only finds where it is. A proposal with neither field is now
// rejected as errEmptyProposal, which says so.
func (ev *evaluator) untargetedFunctionSourceDiff(ctx context.Context, proposal agents.OptimizerResult, hotPaths []string) (string, error) {
	decl, _, err := formatFunctionDecl(proposal.FunctionSource)
	if err != nil {
		return "", err
	}
	name := funcName(decl)
	root, err := ev.baseRoot(ctx)
	if err != nil {
		return "", err
	}
	location, err := locateFunction(root, name, hotPaths)
	if err != nil {
		return "", err
	}
	return ev.buildFunctionSourceDiff(ctx, agents.Target{Function: name, Location: location}, proposal.FunctionSource, proposal.Imports)
}

// locateFunction finds the one non-test declaration of function (in funcName's
// format) under repo and returns its repository-relative location. Vendored
// code, testdata and hidden directories are skipped, like every other walk
// of a target's source.
//
// When several files declare it (gojq has an (*encoder).encodeObject in both
// its root package and cli/), the match in a file the analysis listed as a
// hot path wins, since those are the files the optimizer was shown; only a
// tie that survives that is refused.
func locateFunction(repo, function string, hotPaths []string) (string, error) {
	found, err := declarationsOf(repo, function)
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", function, err)
	}
	if len(found) > 1 {
		found = preferHotFiles(found, hotPaths)
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("function_source declares %s, which no non-test file declares; with no target, return a patch", function)
	default:
		return "", fmt.Errorf("function_source declares %s, which %d files declare (%s); with no target, return a patch", function, len(found), strings.Join(found, ", "))
	}
}

// declarationsOf lists every non-test declaration of function under repo as
// a repository-relative file:line.
func declarationsOf(repo, function string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return skipSourceDir(path, repo, d.Name())
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if line, ok := declaredAt(path, function); ok {
			rel, relErr := filepath.Rel(repo, path)
			if relErr != nil {
				return relErr
			}
			found = append(found, filepath.ToSlash(rel)+":"+strconv.Itoa(line))
		}
		return nil
	})
	return found, err
}

func skipSourceDir(path, repo, name string) error {
	if path == repo {
		return nil
	}
	if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
		return filepath.SkipDir
	}
	return nil
}

// declaredAt reports the line function is declared on in path, if it is.
// A file that does not parse is not a match.
func declaredAt(path, function string) (int, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return 0, false
	}
	if fd := findFuncDecl(file, function); fd != nil {
		return fset.Position(fd.Pos()).Line, true
	}
	return 0, false
}

// errEmptyProposal rejects an optimizer answer that carries no patch and no
// function source, so the record says the model sent nothing rather than
// the generic "patch is empty" a malformed diff also produces.
var errEmptyProposal = errors.New("the optimizer returned neither a patch nor a function_source")

// emptyProposal is errEmptyProposal, or, when the optimizer's call itself
// failed and the graph continued with an empty answer, that failure: on
// overnight-yq-2 a model call that timed out three times was recorded as
// "the optimizer returned neither a patch nor a function_source", which
// blamed an answer that never arrived.
func emptyProposal(roleFailure string) error {
	if roleFailure == "" {
		return errEmptyProposal
	}
	return fmt.Errorf("the optimizer did not answer: %s", roleFailure)
}

// preferHotFiles keeps the locations whose file appears among hotPaths, or
// all of them when none does.
func preferHotFiles(found, hotPaths []string) []string {
	hot := map[string]bool{}
	for _, p := range hotPaths {
		hot[targetPath(p)] = true
	}
	var kept []string
	for _, loc := range found {
		if hot[targetPath(loc)] {
			kept = append(kept, loc)
		}
	}
	if len(kept) == 0 {
		return found
	}
	return kept
}

func hotLocations(paths []agents.HotPath) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.Location)
	}
	return out
}
