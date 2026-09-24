package campaign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
)

// Transport names, recorded on a candidate so a report can say how its diff
// was produced (ADR 0022).
const (
	// PatchTransport is the ordinary optimizer-authored unified diff, and the
	// only transport available when there is no code-chosen target.
	PatchTransport = "patch"
	// FunctionSourceTransport marks a diff deterministic code built from the
	// optimizer's whole replacement function declaration.
	FunctionSourceTransport = "function_source"
)

// resolveCandidatePatch chooses the diff text evaluateCandidate writes and
// applies, and the transport that produced it. A non-empty Patch always wins:
// it is the fallback transport, and an optimizer that ignores its
// instruction still produces something the deterministic gates can judge.
// Without a patch, a code-chosen target and a non-empty FunctionSource build
// the diff deterministically; every other case, including no target at all,
// passes Patch through unchanged (empty or not) exactly as before this
// transport existed.
func (e *Engine) resolveCandidatePatch(ctx context.Context, req orchestrator.CandidateRequest) (patch, transport string, err error) {
	if req.Proposal.Patch != "" {
		return req.Proposal.Patch, PatchTransport, nil
	}
	if req.Target != nil && req.Proposal.FunctionSource != "" {
		diff, err := e.buildFunctionSourceDiff(ctx, *req.Target, req.Proposal.FunctionSource, req.Proposal.Imports)
		if err != nil {
			return "", FunctionSourceTransport, err
		}
		return diff, FunctionSourceTransport, nil
	}
	return req.Proposal.Patch, PatchTransport, nil
}

// buildFunctionSourceDiff reads the target function's file at the campaign's
// base revision, splices functionSource in over the function target.Function
// names, adds any imports it needs that the file does not already have, and
// turns the result into a unified diff via `git diff --no-index`
// (toolchain.DiffFiles). The base revision is read from e.state.Repository,
// the canonical checkout, which is clean at that revision for the duration of
// a candidate's evaluation.
func (e *Engine) buildFunctionSourceDiff(ctx context.Context, target agents.Target, functionSource string, imports []string) (string, error) {
	relPath := targetPath(target.Location)
	if relPath == "" || target.Function == "" {
		return "", errors.New("function_source requires a target with a location and function name")
	}
	fullPath := filepath.Join(e.state.Repository, filepath.FromSlash(relPath))
	original, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", relPath, err)
	}
	updated, err := replaceFunctionSource(fullPath, original, target.Function, functionSource, imports)
	if err != nil {
		return "", err
	}
	return e.diffAgainstBase(ctx, relPath, original, updated)
}

// diffAgainstBase writes original and updated to a scratch directory and asks
// the toolchain for their diff, named at relPath.
func (e *Engine) diffAgainstBase(ctx context.Context, relPath string, original, updated []byte) (string, error) {
	scratch, err := os.MkdirTemp("", "gotorque-function-source-*")
	if err != nil {
		return "", fmt.Errorf("create scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	oldPath := filepath.Join(scratch, "old.go")
	newPath := filepath.Join(scratch, "new.go")
	if err := os.WriteFile(oldPath, original, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(newPath, updated, 0o600); err != nil {
		return "", err
	}
	result, err := e.toolchain.DiffFiles(ctx, oldPath, newPath, relPath)
	if err != nil {
		return "", fmt.Errorf("diff function_source against the base revision: %w", err)
	}
	if len(bytes.TrimSpace(result.Stdout)) == 0 {
		return "", errors.New("function_source is identical to the base revision; nothing to patch")
	}
	return string(result.Stdout), nil
}

// replaceFunctionSource splices functionSource over the declaration of
// function in original, keeping the existing doc comment unless
// functionSource carries its own, then adds any imports the file does not
// already have.
func replaceFunctionSource(fullPath string, original []byte, function, functionSource string, imports []string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fullPath, original, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("%s does not parse at the base revision: %w", filepath.Base(fullPath), err)
	}
	existing := findFuncDecl(file, function)
	if existing == nil {
		return nil, fmt.Errorf("function %s not found in %s", function, filepath.Base(fullPath))
	}
	newDecl, newText, err := formatFunctionDecl(functionSource)
	if err != nil {
		return nil, err
	}
	if name := funcName(newDecl); name != function {
		return nil, fmt.Errorf("function_source declares %s, not the target %s", name, function)
	}
	spliced := spliceFunctionSource(fset, original, existing, newDecl, newText)
	return addImports(fullPath, spliced, imports)
}

// spliceFunctionSource replaces existing's byte range in original with
// newText. The existing doc comment is dropped from the cut range, and so
// from the result, only when newDecl carries its own: otherwise the original
// text is left untouched ahead of the cut and the old doc comment survives
// unedited.
func spliceFunctionSource(fset *token.FileSet, original []byte, existing, newDecl *ast.FuncDecl, newText string) []byte {
	start := existing.Pos()
	if existing.Doc != nil && newDecl.Doc != nil {
		start = existing.Doc.Pos()
	}
	startOff := fset.Position(start).Offset
	endOff := fset.Position(existing.End()).Offset
	var buf bytes.Buffer
	buf.Write(original[:startOff])
	buf.WriteString(strings.TrimRight(newText, "\n"))
	buf.Write(original[endOff:])
	return buf.Bytes()
}

// findFuncDecl returns the top-level function declaration whose funcName
// (path/receiver-qualified, matching internal/campaign/causes.go's format)
// equals function.
func findFuncDecl(file *ast.File, function string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && funcName(fd) == function {
			return fd
		}
	}
	return nil
}

// formatFunctionDecl validates that src parses as exactly one Go function
// declaration, gofmt-formats it in isolation (so the spliced-in text matches
// the target's style regardless of what the model sent), and returns both the
// declaration (for funcName and Doc) and its formatted text, doc comment
// included when it has one.
func formatFunctionDecl(src string) (*ast.FuncDecl, string, error) {
	trimmed := strings.TrimSpace(src)
	if trimmed == "" {
		return nil, "", errors.New("function_source is empty")
	}
	synthetic := "package p\n\n" + trimmed + "\n"
	formatted, err := format.Source([]byte(synthetic))
	if err != nil {
		return nil, "", fmt.Errorf("function_source does not parse as Go source: %w", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", formatted, parser.ParseComments)
	if err != nil {
		return nil, "", fmt.Errorf("function_source does not parse as Go source: %w", err)
	}
	if len(file.Decls) != 1 {
		return nil, "", fmt.Errorf("function_source must declare exactly one function, found %d declarations", len(file.Decls))
	}
	fd, ok := file.Decls[0].(*ast.FuncDecl)
	if !ok {
		return nil, "", errors.New("function_source must be a function declaration")
	}
	start := fd.Pos()
	if fd.Doc != nil {
		start = fd.Doc.Pos()
	}
	text := string(formatted[fset.Position(start).Offset:fset.Position(fd.End()).Offset])
	return fd, text, nil
}

// addImports adds every path in wanted that data does not already import,
// gofmt-sorted into the file's existing import block (or a new one, when it
// has none). Paths already present, including blank spellings, are left
// alone.
func addImports(fullPath string, data []byte, wanted []string) ([]byte, error) {
	if len(wanted) == 0 {
		return data, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fullPath, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("source does not parse after replacing the function: %w", err)
	}
	missing := missingImports(file, wanted)
	if len(missing) == 0 {
		return data, nil
	}
	decl := importDecl(file)
	if decl == nil {
		return insertImportBlock(fset, data, file, missing)
	}
	return replaceImportBlock(fset, data, decl, missing)
}

func missingImports(file *ast.File, wanted []string) []string {
	have := map[string]bool{}
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil {
			have[path] = true
		}
	}
	var missing []string
	for _, imp := range wanted {
		imp = strings.TrimSpace(imp)
		if imp == "" || have[imp] {
			continue
		}
		have[imp] = true
		missing = append(missing, imp)
	}
	return missing
}

func importDecl(file *ast.File) *ast.GenDecl {
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			return gd
		}
	}
	return nil
}

// replaceImportBlock rebuilds decl's import specs, existing ones plus
// missing, gofmt-sorted, and splices the result over decl's byte range.
// Rebuilding from scratch rather than editing decl's text in place loses any
// blank-line grouping the original block had; format.Source still produces a
// valid, sorted single group, and the file still builds.
func replaceImportBlock(fset *token.FileSet, data []byte, decl *ast.GenDecl, missing []string) ([]byte, error) {
	specs := make([]string, 0, len(decl.Specs)+len(missing))
	for _, spec := range decl.Specs {
		imp, ok := spec.(*ast.ImportSpec)
		if !ok {
			continue
		}
		text := imp.Path.Value
		if imp.Name != nil {
			text = imp.Name.Name + " " + text
		}
		specs = append(specs, text)
	}
	for _, imp := range missing {
		specs = append(specs, strconv.Quote(imp))
	}
	block, err := formatImportBlock(specs)
	if err != nil {
		return nil, err
	}
	start := fset.Position(decl.Pos()).Offset
	end := fset.Position(decl.End()).Offset
	var buf bytes.Buffer
	buf.Write(data[:start])
	buf.WriteString(block)
	buf.Write(data[end:])
	return buf.Bytes(), nil
}

// insertImportBlock adds a fresh import block for missing right after the
// package clause, for a file that had no import declaration at all.
func insertImportBlock(fset *token.FileSet, data []byte, file *ast.File, missing []string) ([]byte, error) {
	specs := make([]string, 0, len(missing))
	for _, imp := range missing {
		specs = append(specs, strconv.Quote(imp))
	}
	block, err := formatImportBlock(specs)
	if err != nil {
		return nil, err
	}
	at := fset.Position(file.Name.End()).Offset
	var buf bytes.Buffer
	buf.Write(data[:at])
	buf.WriteString("\n\n")
	buf.WriteString(strings.TrimRight(block, "\n"))
	buf.Write(data[at:])
	return buf.Bytes(), nil
}

// formatImportBlock renders specs as a gofmt-sorted `import (...)` block by
// formatting a synthetic file and extracting the block back out; format.Source
// is what sorts import groups (go/format).
func formatImportBlock(specs []string) (string, error) {
	var b strings.Builder
	b.WriteString("package p\n\nimport (\n")
	for _, s := range specs {
		b.WriteString("\t" + s + "\n")
	}
	b.WriteString(")\n")
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return "", fmt.Errorf("could not format import block: %w", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", formatted, parser.ParseComments)
	if err != nil {
		return "", err
	}
	decl := importDecl(file)
	if decl == nil {
		return "", errors.New("formatted import block lost its import declaration")
	}
	start := fset.Position(decl.Pos()).Offset
	end := fset.Position(decl.End()).Offset
	return string(formatted[start:end]), nil
}
