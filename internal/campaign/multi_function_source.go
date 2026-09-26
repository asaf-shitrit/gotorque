package campaign

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"example.com/gotorque/internal/agents"
)

// parsedFunctionSource is one of the optimizer's function_sources entries,
// parsed and gofmt-formatted in isolation by formatFunctionDecl.
type parsedFunctionSource struct {
	name string
	fd   *ast.FuncDecl
	text string
}

// buildMultiFunctionSourceDiff turns several whole function declarations into
// one multi-file unified diff for a throwaway_result target (ADR 0027). Every
// declaration whose name matches the callee or one of target.Callers
// replaces that function, in whichever repository file it is declared in.
// A declaration whose name matches neither, and does not exist anywhere else
// in the callee's package, is a wholly new function, appended to the end of
// the callee's own declaration in the callee's own file. A declaration whose
// name exists elsewhere in the package but outside the set is a rejection:
// letting it through would let the optimizer rewrite a function the set,
// and so the shape check, never agreed to.
func (e *Engine) buildMultiFunctionSourceDiff(ctx context.Context, target agents.Target, sources []string, imports []string) (string, error) {
	calleeRel := targetPath(target.Location)
	if calleeRel == "" || target.Function == "" {
		return "", errors.New("function_sources requires a target with a location and function name")
	}
	decls, err := parseFunctionSources(sources)
	if err != nil {
		return "", err
	}
	byFile, err := groupByFile(e.state.Repository, calleeRel, knownFunctions(target, calleeRel), decls)
	if err != nil {
		return "", err
	}
	diff, changedAny, err := e.diffEveryFile(ctx, byFile, target.Function, calleeRel, imports)
	if err != nil {
		return "", err
	}
	if !changedAny {
		return "", errors.New("function_sources produced no change against the base revision")
	}
	return diff, nil
}

// knownFunctions maps every name in target's set, callee included, to the
// repository-relative file it is declared in.
func knownFunctions(target agents.Target, calleeRel string) map[string]string {
	known := map[string]string{target.Function: calleeRel}
	for _, f := range target.Callers {
		known[f.Name] = targetPath(f.Location)
	}
	return known
}

// parseFunctionSources parses and gofmt-formats every source in isolation,
// reusing formatFunctionDecl's single-declaration validation.
func parseFunctionSources(sources []string) ([]parsedFunctionSource, error) {
	decls := make([]parsedFunctionSource, 0, len(sources))
	for _, src := range sources {
		fd, text, err := formatFunctionDecl(src)
		if err != nil {
			return nil, err
		}
		decls = append(decls, parsedFunctionSource{name: funcName(fd), fd: fd, text: text})
	}
	return decls, nil
}

// diffEveryFile builds and concatenates the diff for each file byFile names,
// in a deterministic (sorted) order, and reports whether any file actually
// changed.
func (e *Engine) diffEveryFile(ctx context.Context, byFile map[string][]parsedFunctionSource, calleeFunction, calleeRel string, imports []string) (string, bool, error) {
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	slices.Sort(files)
	var out strings.Builder
	changedAny := false
	for _, rel := range files {
		diff, changed, err := e.buildOneFileDiff(ctx, rel, calleeFunction, calleeRel, byFile[rel], imports)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", rel, err)
		}
		if changed {
			out.WriteString(diff)
			changedAny = true
		}
	}
	return out.String(), changedAny, nil
}

// groupByFile resolves each declaration to the repository file it belongs
// in: known's file for a name in the set, the callee's own file for a name
// found nowhere else in the package, and a rejection for a name that exists
// elsewhere in the package but outside the set.
func groupByFile(repo, calleeRel string, known map[string]string, decls []parsedFunctionSource) (map[string][]parsedFunctionSource, error) {
	byFile := map[string][]parsedFunctionSource{}
	for _, d := range decls {
		rel, ok := known[d.name]
		if !ok {
			if existsElsewhereInPackage(repo, calleeRel, d.name) {
				return nil, fmt.Errorf("function_sources declares %s, which exists elsewhere in the package but is outside the throwaway_result set; add it to the set or leave it unchanged", d.name)
			}
			rel = calleeRel
		}
		byFile[rel] = append(byFile[rel], d)
	}
	return byFile, nil
}

// existsElsewhereInPackage reports whether name is declared by any non-test
// .go file in calleeRel's directory.
func existsElsewhereInPackage(repo, calleeRel, name string) bool {
	dir := filepath.Join(repo, filepath.Dir(filepath.FromSlash(calleeRel)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		if findFuncDecl(f, name) != nil {
			return true
		}
	}
	return false
}

// sourceEdit is a byte-range replacement (start==end for a pure insertion)
// against a file's original content.
type sourceEdit struct {
	start, end int
	text       string
}

// applyEdits applies every edit against original, processing them in
// descending order of start offset so that offsets computed against the
// original file stay valid regardless of how much earlier edits grow or
// shrink the text before them.
func applyEdits(original []byte, edits []sourceEdit) []byte {
	sorted := slices.Clone(edits)
	slices.SortFunc(sorted, func(a, b sourceEdit) int { return cmp.Compare(b.start, a.start) })
	buf := append([]byte(nil), original...)
	for _, e := range sorted {
		next := make([]byte, 0, len(buf)-(e.end-e.start)+len(e.text))
		next = append(next, buf[:e.start]...)
		next = append(next, e.text...)
		next = append(next, buf[e.end:]...)
		buf = next
	}
	return buf
}

// buildOneFileDiff replaces every known declaration in rel and appends every
// wholly new one after the callee's declaration (rel must be the callee's own
// file for a new declaration to be valid; buildFileEdits enforces that), then
// runs the same import bookkeeping buildFunctionSourceDiff uses: imports is
// added only to the callee's own file (the only file the optimizer's brief
// names imports for), and dropOrphanedImports runs against every touched
// file. changed is false, with no error, when rel's content after every edit
// is byte-identical to its original -- a file diffAgainstBase would refuse
// rather than skip.
func (e *Engine) buildOneFileDiff(ctx context.Context, rel, calleeFunction, calleeRel string, decls []parsedFunctionSource, imports []string) (diff string, changed bool, err error) {
	fullPath := filepath.Join(e.state.Repository, filepath.FromSlash(rel))
	original, err := os.ReadFile(fullPath)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", rel, err)
	}
	final, changed, err := spliceOneFile(fullPath, rel, calleeFunction, calleeRel, original, decls, imports)
	if err != nil || !changed {
		return "", false, err
	}
	diffText, err := e.diffAgainstBase(ctx, rel, original, final)
	if err != nil {
		return "", false, err
	}
	return diffText, true, nil
}

// spliceOneFile applies decls' edits (and, for the callee's own file, any
// missing imports) against original, and drops any import the edits left
// unused. changed is false, with no error, when there is nothing to splice or
// the result is byte-identical to original.
func spliceOneFile(fullPath, rel, calleeFunction, calleeRel string, original []byte, decls []parsedFunctionSource, imports []string) ([]byte, bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fullPath, original, parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("%s does not parse at the base revision: %w", filepath.Base(fullPath), err)
	}
	edits, err := buildFileEdits(fset, file, rel, calleeFunction, calleeRel, decls)
	if err != nil {
		return nil, false, err
	}
	if len(edits) == 0 {
		return nil, false, nil
	}
	updated := applyEdits(original, edits)
	if rel == calleeRel && len(imports) > 0 {
		updated, err = addImports(fullPath, updated, imports)
		if err != nil {
			return nil, false, err
		}
	}
	final, err := dropOrphanedImports(fullPath, original, updated)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(original, final) {
		return nil, false, nil
	}
	return final, true, nil
}

// buildFileEdits turns decls into a splice-or-insert edit list against
// file's byte offsets: a declaration matching an existing FuncDecl splices
// over it (keeping the old doc comment unless the replacement carries its
// own, exactly as spliceFunctionSource does for the single-function
// transport); a declaration matching nothing existing is new, and is only
// valid when rel is the callee's own file, where it is inserted immediately
// after the callee's own declaration. groupByFile always resolves an
// unrecognized name to the callee's own file, so the "wrong file" error
// below is unreachable through buildMultiFunctionSourceDiff today; it stays
// as a defensive check for a future caller (or a groupByFile change) that
// stops guaranteeing that.
func buildFileEdits(fset *token.FileSet, file *ast.File, rel, calleeFunction, calleeRel string, decls []parsedFunctionSource) ([]sourceEdit, error) {
	var edits []sourceEdit
	var fresh []parsedFunctionSource
	for _, d := range decls {
		existing := findFuncDecl(file, d.name)
		if existing == nil {
			fresh = append(fresh, d)
			continue
		}
		start := existing.Pos()
		if existing.Doc != nil && d.fd.Doc != nil {
			start = existing.Doc.Pos()
		}
		edits = append(edits, sourceEdit{
			start: fset.Position(start).Offset,
			end:   fset.Position(existing.End()).Offset,
			text:  strings.TrimRight(d.text, "\n"),
		})
	}
	if len(fresh) == 0 {
		return edits, nil
	}
	if rel != calleeRel {
		return nil, fmt.Errorf("function_sources declares %s as a new function, but new functions may only be added to the callee's own file (%s)", fresh[0].name, calleeRel)
	}
	calleeDecl := findFuncDecl(file, calleeFunction)
	if calleeDecl == nil {
		return nil, fmt.Errorf("callee %s not found in %s", calleeFunction, filepath.Base(rel))
	}
	at := fset.Position(calleeDecl.End()).Offset
	var b strings.Builder
	for _, nf := range fresh {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(nf.text, "\n"))
	}
	edits = append(edits, sourceEdit{start: at, end: at, text: b.String()})
	return edits, nil
}
