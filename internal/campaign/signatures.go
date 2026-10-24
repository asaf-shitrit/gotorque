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
	dir := filepath.Join(repo, filepath.Dir(filepath.FromSlash(rel)))
	files := packageFiles(fset, dir)
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
	ctx := contextBuilder{fset: fset, seen: map[string]bool{}, budget: maxSignatures}
	own := indexPackage(files, fd)
	pkgs := newFirstPartyPackages(fset, dir, home)
	declarationsFor(&ctx, own, fd)
	targetValueTypes(&ctx, typeResolver{own: own, pkgs: pkgs}, fd)
	firstPartyDeclarations(&ctx, pkgs, fd)
	return ctx.out
}

// contextBuilder collects the declarations a target carries, in order, with
// one shared budget and one shared dedupe set: three passes feed it, and a
// declaration any two of them reach belongs in the context once.
type contextBuilder struct {
	fset   *token.FileSet
	seen   map[string]bool
	out    []string
	budget int
}

// add appends text unless it is empty, already there, or the budget is full.
func (b *contextBuilder) add(text string) {
	if text == "" || b.seen[text] || len(b.out) >= b.budget {
		return
	}
	b.seen[text] = true
	b.out = append(b.out, text)
}

// full reports whether the budget is spent, which stops a pass early.
func (b *contextBuilder) full() bool { return len(b.out) >= b.budget }

// addDecl appends a function or type declaration, gofmt-formatted and cut at
// the same bounds wherever it came from.
func (b *contextBuilder) addDecl(node ast.Node, prefix string) {
	b.add(prefix + renderDecl(b.fset, node))
}

// packageIndex indexes one package's declarations by name, and its methods by
// receiver base type: a value's type name is what both a declaration and a
// method call resolve through.
type packageIndex struct {
	decls   map[string][]ast.Node
	methods map[string]map[string]*ast.FuncDecl
}

func indexPackage(files []*ast.File, skip *ast.FuncDecl) packageIndex {
	return packageIndex{decls: packageDecls(files, skip), methods: methodIndex(files, skip)}
}

// typeSpec returns the type declaration of that name, or nil.
func (p packageIndex) typeSpec(name string) *ast.TypeSpec {
	for _, node := range p.decls[name] {
		if ts, ok := node.(*ast.TypeSpec); ok {
			return ts
		}
	}
	return nil
}

// method returns the method of that name on the receiver base type, or nil.
func (p packageIndex) method(base, name string) *ast.FuncDecl {
	return p.methods[base][name]
}

// methodIndex indexes a package's methods by receiver base type and name.
func methodIndex(files []*ast.File, skip *ast.FuncDecl) map[string]map[string]*ast.FuncDecl {
	methods := map[string]map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd == skip || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			base := baseTypeName(fd.Recv.List[0].Type)
			if base == "" {
				continue
			}
			if methods[base] == nil {
				methods[base] = map[string]*ast.FuncDecl{}
			}
			methods[base][fd.Name.Name] = fd
		}
	}
	return methods
}

// baseTypeName is the declared type name a type expression names: T, *T, and
// the T of a generic T[P] receiver.
func baseTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return baseTypeName(e.X)
	case *ast.IndexExpr:
		return baseTypeName(e.X)
	case *ast.IndexListExpr:
		return baseTypeName(e.X)
	}
	return ""
}

// valueType is a named type a target's value holds, with the package that
// declares it: resolving a value's type is what tells the optimizer the
// fields and methods it actually has.
type valueType struct {
	index packageIndex
	name  string
	label string
}

// typeResolver resolves an expression to the named type it holds, in the
// target's own package or in a first-party package it imports. Resolution is
// AST-based, over the packages' own declarations: go/types would mean loading
// the target's module, which the context collector cannot do offline, and
// this only ever adds advice to a prompt.
type typeResolver struct {
	own  packageIndex
	pkgs firstPartyPackages
}

func (r typeResolver) resolve(expr ast.Expr) (valueType, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		return r.named(r.own, "", e.Name)
	case *ast.StarExpr:
		return r.resolve(e.X)
	case *ast.ParenExpr:
		return r.resolve(e.X)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return r.resolve(e.X)
		}
	case *ast.CompositeLit:
		return r.resolve(e.Type)
	case *ast.CallExpr:
		return r.callResult(e)
	}
	return valueType{}, false
}

// named returns the value type when index declares that type.
func (r typeResolver) named(index packageIndex, label, name string) (valueType, bool) {
	if index.typeSpec(name) == nil {
		return valueType{}, false
	}
	return valueType{index: index, name: name, label: label}, true
}

// callResult resolves a call to the type of its single result, which is how a
// target gets a value whose type name never appears in its body: on jj the
// target main got `func parseArgs() args` and not the args struct, invented
// fields on it, and failed to build. A conversion and new() name their type
// directly.
func (r typeResolver) callResult(call *ast.CallExpr) (valueType, bool) {
	if id, ok := call.Fun.(*ast.Ident); ok {
		if id.Name == "new" && len(call.Args) == 1 {
			return r.resolve(call.Args[0])
		}
		if vt, isType := r.named(r.own, "", id.Name); isType && len(call.Args) == 1 {
			return vt, true
		}
		return r.result(r.own, "", id.Name)
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return valueType{}, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return valueType{}, false
	}
	other := r.pkgs.index(pkg.Name)
	if other.decls == nil {
		return valueType{}, false
	}
	return r.result(other, "// package "+pkg.Name+"\n", sel.Sel.Name)
}

// result returns the declared type of the first result of index's funcs name,
// when that function has exactly one result.
func (r typeResolver) result(index packageIndex, label, name string) (valueType, bool) {
	for _, node := range index.decls[name] {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
			continue
		}
		if vt, isType := r.named(index, label, baseTypeName(fd.Type.Results.List[0].Type)); isType {
			return vt, true
		}
	}
	return valueType{}, false
}

// localValueTypes maps the names of fd's receiver, its parameters and its
// local values to the named type each holds.
func localValueTypes(fd *ast.FuncDecl, r typeResolver) map[string]valueType {
	types := map[string]valueType{}
	addFields(types, fd.Recv, r)
	addFields(types, fd.Type.Params, r)
	if fd.Body == nil {
		return types
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.ValueSpec:
			addValueSpec(types, s, r)
		case *ast.AssignStmt:
			addAssign(types, s, r)
		}
		return true
	})
	return types
}

// addFields records the types of a receiver or parameter list, which the
// target's body never names.
func addFields(types map[string]valueType, fields *ast.FieldList, r typeResolver) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		vt, ok := r.resolve(field.Type)
		if !ok {
			continue
		}
		for _, name := range field.Names {
			types[name.Name] = vt
		}
	}
}

// addValueSpec records one declared variable, from its explicit type or from
// the value it is declared with.
func addValueSpec(types map[string]valueType, spec *ast.ValueSpec, r typeResolver) {
	if len(spec.Names) != 1 {
		return
	}
	expr := spec.Type
	if expr == nil && len(spec.Values) == 1 {
		expr = spec.Values[0]
	}
	if expr == nil {
		return
	}
	if vt, ok := r.resolve(expr); ok {
		types[spec.Names[0].Name] = vt
	}
}

// addAssign records a single-value assignment, which is how a call's result
// reaches a variable.
func addAssign(types map[string]valueType, stmt *ast.AssignStmt, r typeResolver) {
	if len(stmt.Lhs) != 1 || len(stmt.Rhs) != 1 {
		return
	}
	id, ok := stmt.Lhs[0].(*ast.Ident)
	if !ok {
		return
	}
	if vt, ok := r.resolve(stmt.Rhs[0]); ok {
		types[id.Name] = vt
	}
}

// targetValueTypes renders, in the order the target first reads them, the
// declared type of each first-party value it reads through a selector and the
// methods it calls on that value. Target context collected by referenced
// names misses both: a value from a call carries a type its body never spells,
// and a method call on it is not a package-level declaration at all. On tomlv
// the patch called .String() on a value that already was a string, which the
// type's own method set would have shown; on jj the candidate invented fields
// on an args struct it never saw.
func targetValueTypes(ctx *contextBuilder, r typeResolver, fd *ast.FuncDecl) {
	if fd.Body == nil || ctx.full() {
		return
	}
	values := localValueTypes(fd, r)
	for _, ref := range selectorRefs(fd.Body) {
		vt, ok := values[ref[0]]
		if !ok {
			continue
		}
		addValueType(ctx, vt)
		if method := vt.index.method(vt.name, ref[1]); method != nil {
			ctx.addDecl(method, vt.label)
		}
		if ctx.full() {
			return
		}
	}
}

// addValueType appends a value's declared type, which carries its fields.
func addValueType(ctx *contextBuilder, vt valueType) {
	if spec := vt.index.typeSpec(vt.name); spec != nil {
		ctx.addDecl(spec, vt.label)
	}
}

// firstPartyDeclarations renders the declarations fd reaches through a
// package selector (types.RecordAndContext) when that package belongs to the
// target's own module. On overnight-miller-3 a getRecordBatch rewrite failed
// to build over a field of types.RecordAndContext, which lives in miller's
// pkg/types, not in the target's package. Standard-library and third-party
// packages are left out: their APIs are what the model knows best, and
// resolving them would mean reading the module cache.
func firstPartyDeclarations(ctx *contextBuilder, pkgs firstPartyPackages, fd *ast.FuncDecl) {
	if ctx.full() || pkgs.module == "" {
		return
	}
	for _, ref := range selectorRefs(fd) {
		for _, node := range pkgs.index(ref[0]).decls[ref[1]] {
			ctx.addDecl(node, "// package "+ref[0]+"\n")
			if ctx.full() {
				return
			}
		}
	}
}

// firstPartyPackages resolves a file's import names to the declarations of
// the packages that belong to its module, parsing each package once.
type firstPartyPackages struct {
	fset         *token.FileSet
	root, module string
	imports      map[string]string
	cache        map[string]packageIndex
}

func newFirstPartyPackages(fset *token.FileSet, dir string, file *ast.File) firstPartyPackages {
	root, module := moduleOf(dir)
	return firstPartyPackages{
		fset:    fset,
		root:    root,
		module:  module,
		imports: importsByName(file),
		cache:   map[string]packageIndex{},
	}
}

// index returns the declarations of the package the file imports as name, or
// the zero index when it imports no such package or the package is not
// first-party.
func (p firstPartyPackages) index(name string) packageIndex {
	path, ok := p.imports[name]
	if !ok || p.module == "" || (path != p.module && !strings.HasPrefix(path, p.module+"/")) {
		return packageIndex{}
	}
	if index, cached := p.cache[path]; cached {
		return index
	}
	dir := filepath.Join(p.root, filepath.FromSlash(strings.TrimPrefix(path, p.module)))
	index := indexPackage(packageFiles(p.fset, dir), nil)
	p.cache[path] = index
	return index
}

// selectorRefs lists the (name, selector) pairs of a node's selectors on a
// bare identifier, first use first. A local variable's field access looks the
// same; it simply matches no import.
func selectorRefs(node ast.Node) [][2]string {
	var refs [][2]string
	seen := map[[2]string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
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
func declarationsFor(ctx *contextBuilder, index packageIndex, fd *ast.FuncDecl) {
	for _, name := range referencedNames(fd) {
		for _, node := range index.decls[name] {
			ctx.addDecl(node, "")
			if ctx.full() {
				return
			}
		}
	}
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
