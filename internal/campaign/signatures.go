package campaign

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/agents"
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
	var home *ast.File
	for _, f := range files {
		if fd = findFuncDecl(f, target.Function); fd != nil {
			home = f
			break
		}
	}
	if fd == nil || fd.Body == nil {
		return nil
	}
	out := declarationsFor(fset, files, fd)
	dir := filepath.Join(repo, filepath.Dir(filepath.FromSlash(rel)))
	return append(out, firstPartyDeclarations(fset, dir, home, fd, maxSignatures-len(out))...)
}

// firstPartyDeclarations renders the declarations fd reaches through a
// package selector (types.RecordAndContext) when that package belongs to the
// target's own module. On overnight-miller-3 a getRecordBatch rewrite failed
// to build over a field of types.RecordAndContext, which lives in miller's
// pkg/types, not in the target's package. Standard-library and third-party
// packages are left out: their APIs are what the model knows best, and
// resolving them would mean reading the module cache.
func firstPartyDeclarations(fset *token.FileSet, dir string, file *ast.File, fd *ast.FuncDecl, budget int) []string {
	root, module := moduleOf(dir)
	if budget <= 0 || module == "" {
		return nil
	}
	pkgs := firstPartyPackages{fset: fset, root: root, module: module, imports: importsByName(file), cache: map[string]map[string][]ast.Node{}}
	var out []string
	for _, ref := range selectorRefs(fd) {
		for _, node := range pkgs.decls(ref[0])[ref[1]] {
			if len(out) == budget {
				return out
			}
			if text := renderDecl(fset, node); text != "" {
				out = append(out, "// package "+ref[0]+"\n"+text)
			}
		}
	}
	return out
}

// firstPartyPackages resolves a file's import names to the declarations of
// the packages that belong to its module, parsing each package once.
type firstPartyPackages struct {
	fset         *token.FileSet
	root, module string
	imports      map[string]string
	cache        map[string]map[string][]ast.Node
}

// decls returns the declarations of the package the file imports as name,
// or nil when it imports no such package or the package is not first-party.
func (p firstPartyPackages) decls(name string) map[string][]ast.Node {
	path, ok := p.imports[name]
	if !ok || (path != p.module && !strings.HasPrefix(path, p.module+"/")) {
		return nil
	}
	if decls, cached := p.cache[path]; cached {
		return decls
	}
	decls := packageDecls(packageFiles(p.fset, filepath.Join(p.root, filepath.FromSlash(strings.TrimPrefix(path, p.module)))), nil)
	p.cache[path] = decls
	return decls
}

// selectorRefs lists the (package, name) pairs of fd's selectors on a bare
// identifier, first use first. A local variable's field access looks the
// same; it simply matches no import.
func selectorRefs(fd *ast.FuncDecl) [][2]string {
	var refs [][2]string
	seen := map[[2]string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, isIdent := sel.X.(*ast.Ident); isIdent {
			ref := [2]string{id.Name, sel.Sel.Name}
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
		return true
	})
	return refs
}

// importsByName maps the names file refers to its imports by to their paths.
func importsByName(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range file.Imports {
		if name := importName(spec); name != "" {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil {
				out[name] = p
			}
		}
	}
	return out
}

// moduleOf finds the nearest go.mod at or above dir and returns its
// directory and module path, or "" when there is none.
func moduleOf(dir string) (string, string) {
	for d := dir; ; d = filepath.Dir(d) {
		if data, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil {
			return d, modulePathOf(data)
		}
		if parent := filepath.Dir(d); parent == d {
			return "", ""
		}
	}
}

func modulePathOf(gomod []byte) string {
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// packageFiles parses the non-test Go files directly in dir that this
// platform builds. A package can declare a function once per build-tag
// variant: klauspost/compress's internal/race has WriteSlice and ReadSlice in
// both a race and a !race file, so every variant was rendered, WriteSlice
// filled the context budget twice, and the ReadSlice the target also calls
// was never shown; the optimizer then invented a "trace" package for both.
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
		if ok, err := build.Default.MatchFile(dir, name); err != nil || !ok {
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
