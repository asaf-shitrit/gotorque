package campaign

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"example.com/gotorque/internal/agents"
)

// maxSignatures bounds how many declarations a target carries.
const maxSignatures = 12

// maxTypeLines bounds one type declaration's text; a longer one is cut there.
const maxTypeLines = 16

// targetSignatures lists the declarations, from the target function's own
// package, of the functions, methods and types its body refers to, plus its
// receiver's type: signatures without bodies, and type definitions. The
// optimizer sees only the target's source, and in a larger codebase it
// guessed the types around it: on overnight-miller-2 two attempts at
// (*Mlrmap).marshalJSONAuxMultiline failed to build because a value it
// treated as []byte was a string, and on overnight-miller-1 it called a
// method (arena.Finalize) that does not exist. Carrying the real declarations
// costs a few hundred bytes of prompt. They are advice; nothing here decides.
func targetSignatures(repo string, target agents.Target) []string {
	rel := targetPath(target.Location)
	if rel == "" || target.Function == "" {
		return nil
	}
	fset := token.NewFileSet()
	files := packageFiles(fset, filepath.Join(repo, filepath.Dir(filepath.FromSlash(rel))))
	var fd *ast.FuncDecl
	for _, f := range files {
		if fd = findFuncDecl(f, target.Function); fd != nil {
			break
		}
	}
	if fd == nil || fd.Body == nil {
		return nil
	}
	return declarationsFor(fset, files, fd)
}

// packageFiles parses the non-test Go files directly in dir.
func packageFiles(fset *token.FileSet, dir string) []*ast.File {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution); err == nil {
			files = append(files, f)
		}
	}
	return files
}

// declarationsFor renders, in the order the target first refers to them, the
// package-level declarations named in fd's receiver and body.
func declarationsFor(fset *token.FileSet, files []*ast.File, fd *ast.FuncDecl) []string {
	order := referencedNames(fd)
	decls := packageDecls(files, fd)
	var out []string
	seen := map[ast.Node]bool{}
	for _, name := range order {
		for _, node := range decls[name] {
			if seen[node] || len(out) == maxSignatures {
				continue
			}
			seen[node] = true
			if text := renderDecl(fset, node); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// referencedNames is every identifier in fd's receiver type and body, first
// use first, which is how declarations are ordered.
func referencedNames(fd *ast.FuncDecl) []string {
	var names []string
	seen := map[string]bool{}
	add := func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !seen[id.Name] {
				seen[id.Name] = true
				names = append(names, id.Name)
			}
			return true
		})
	}
	if fd.Recv != nil {
		add(fd.Recv)
	}
	add(fd.Body)
	return names
}

// packageDecls indexes the package's functions, methods and types by name,
// leaving out fd itself.
func packageDecls(files []*ast.File, fd *ast.FuncDecl) map[string][]ast.Node {
	decls := map[string][]ast.Node{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d != fd {
					decls[d.Name.Name] = append(decls[d.Name.Name], d)
				}
			case *ast.GenDecl:
				indexTypes(decls, d)
			}
		}
	}
	return decls
}

func indexTypes(decls map[string][]ast.Node, d *ast.GenDecl) {
	if d.Tok != token.TYPE {
		return
	}
	for _, spec := range d.Specs {
		if ts, ok := spec.(*ast.TypeSpec); ok {
			decls[ts.Name.Name] = append(decls[ts.Name.Name], ts)
		}
	}
}

// renderDecl gofmt-formats a function's signature without its body, or a type
// declaration cut at maxTypeLines.
func renderDecl(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	switch n := node.(type) {
	case *ast.FuncDecl:
		if format.Node(&buf, fset, &ast.FuncDecl{Recv: n.Recv, Name: n.Name, Type: n.Type}) != nil {
			return ""
		}
		return buf.String()
	case *ast.TypeSpec:
		if format.Node(&buf, fset, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{n}}) != nil {
			return ""
		}
		return cutLines(buf.String(), maxTypeLines)
	}
	return ""
}

func cutLines(text string, limit int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= limit {
		return text
	}
	return strings.Join(lines[:limit], "\n") + "\n\t// ... cut"
}
