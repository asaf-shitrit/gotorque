package campaign

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// dropOrphanedImports removes each import that the replaced function took the
// file's last use of: one original referred to and updated no longer does. A
// remedy that swaps fmt.Sprintf for concatenation leaves fmt unused, and Go
// refuses to build a file with an unused import, so a correct candidate failed
// the build. Only imports original used are candidates, so a guessed package
// name that is wrong can only keep an import, never drop a used one.
func dropOrphanedImports(fullPath string, original, updated []byte) ([]byte, error) {
	origFile, err := parser.ParseFile(token.NewFileSet(), fullPath, original, 0)
	if err != nil {
		return nil, fmt.Errorf("%s does not parse at the base revision: %w", filepath.Base(fullPath), err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fullPath, updated, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("source does not parse after replacing the function: %w", err)
	}
	decl := importDecl(file)
	if decl == nil {
		return updated, nil
	}
	orphaned := orphanedImports(decl, packageRefs(origFile), packageRefs(file))
	if len(orphaned) == 0 {
		return updated, nil
	}
	start := fset.Position(decl.Pos()).Offset
	end := fset.Position(decl.End()).Offset
	block, err := importDeclWithout(fset, updated, decl, orphaned)
	if err != nil {
		return nil, err
	}
	if block == "" {
		end = skipNewlines(updated, end)
	}
	var buf bytes.Buffer
	buf.Write(updated[:start])
	buf.WriteString(block)
	buf.Write(updated[end:])
	return buf.Bytes(), nil
}

// orphanedImports looks only at the first import declaration, the one
// addImports writes to; a file with several is rare and keeps the rest as is.
func orphanedImports(decl *ast.GenDecl, before, after map[string]bool) map[*ast.ImportSpec]bool {
	orphaned := map[*ast.ImportSpec]bool{}
	for _, s := range decl.Specs {
		spec := s.(*ast.ImportSpec)
		name := importName(spec)
		if name != "" && before[name] && !after[name] {
			orphaned[spec] = true
		}
	}
	return orphaned
}

// packageRefs names every identifier used as the package in a qualified
// reference (pkg.Name). The parser resolves locals, parameters and fields to
// their declarations, so an unresolved selector base is a package name.
func packageRefs(file *ast.File) map[string]bool {
	refs := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, isIdent := sel.X.(*ast.Ident); isIdent && id.Obj == nil {
				refs[id.Name] = true
			}
		}
		return true
	})
	return refs
}

// importName is the name a file refers to an import by: its explicit name, or
// a guess from the path's last element. Blank, dot and cgo imports have no
// name to check and are never dropped.
func importName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil || p == "C" {
		return ""
	}
	return packageNameForPath(p)
}

// packageNameForPath is importName's path guess, factored out so
// multi_function_source.go's per-file import placement (ADR 0027) can guess a
// name for a path that has no ast.ImportSpec yet -- one the optimizer's
// imports list named but no file has added.
func packageNameForPath(p string) string {
	elem := path.Base(p)
	if majorVersion.MatchString(elem) {
		elem = path.Base(path.Dir(p))
	}
	if i := strings.Index(elem, ".v"); i > 0 {
		elem = elem[:i]
	}
	return strings.TrimPrefix(elem, "go-")
}

// importDeclWithout returns decl's text without the orphaned specs' lines,
// gofmt-formatted, or "" when no spec is left.
func importDeclWithout(fset *token.FileSet, data []byte, decl *ast.GenDecl, orphaned map[*ast.ImportSpec]bool) (string, error) {
	if len(orphaned) == len(decl.Specs) {
		return "", nil
	}
	start := fset.Position(decl.Pos()).Offset
	text := data[start:fset.Position(decl.End()).Offset]
	var kept []byte
	last := 0
	for _, s := range decl.Specs {
		spec := s.(*ast.ImportSpec)
		if !orphaned[spec] {
			continue
		}
		from := lineStart(text, fset.Position(spec.Pos()).Offset-start)
		to := lineEnd(text, fset.Position(spec.End()).Offset-start)
		kept = append(kept, text[last:from]...)
		last = to
	}
	kept = append(kept, text[last:]...)
	return formatImportDecl(string(kept))
}

func lineStart(text []byte, at int) int {
	return bytes.LastIndexByte(text[:at], '\n') + 1
}

func lineEnd(text []byte, at int) int {
	if i := bytes.IndexByte(text[at:], '\n'); i >= 0 {
		return at + i + 1
	}
	return len(text)
}

func skipNewlines(data []byte, at int) int {
	for at < len(data) && data[at] == '\n' {
		at++
	}
	return at
}
