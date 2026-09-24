package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// inMemoryTypes are the destinations a read or write call cannot turn into a
// system call: buffers in memory, and the bufio types that already batch.
var inMemoryTypes = map[string]bool{
	"bytes.Buffer": true, "strings.Builder": true, "strings.Reader": true, "bytes.Reader": true,
	"bufio.Writer": true, "bufio.Reader": true, "bufio.Scanner": true,
}

// ioMethods are the calls that read or write through their receiver.
var ioMethods = map[string]bool{
	"Write": true, "WriteByte": true, "WriteString": true, "WriteRune": true, "WriteTo": true,
	"Read": true, "ReadByte": true, "ReadRune": true, "ReadString": true, "ReadBytes": true, "ReadFrom": true,
}

// onlyInMemoryIO reports whether every read and write call in the function is
// provably made on an in-memory buffer or an already-buffered bufio value,
// with at least one such call. Jev reads the source alone and cannot tell
// `e.w.WriteByte` on a *bytes.Buffer field from a write to a file: on gojq it
// flagged (*encoder).writeByte for unbuffered I/O at +2.9 sd, and two of a
// campaign's three candidates went to a function that makes no system call.
//
// A destination whose type cannot be resolved from the receiver's struct,
// the parameters, or a local declaration counts as real I/O, so the answer is
// false whenever there is doubt and Jev's flag stands.
func onlyInMemoryIO(repo string, site hotFunction) bool {
	full := filepath.Join(repo, filepath.FromSlash(site.Path))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	fd := findFunc(file, site.Name)
	if fd == nil || fd.Body == nil {
		return false
	}
	types := localTypes(fd)
	if recv, typeName := receiverOf(fd); recv != "" {
		for field, t := range structFields(filepath.Dir(full), file.Name.Name, typeName) {
			types[recv+"."+field] = t
		}
	}
	calls, inMemory := 0, true
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		dst, ok := ioDestination(call)
		if !ok {
			return true
		}
		calls++
		if !inMemoryTypes[types[exprKey(dst)]] {
			inMemory = false
		}
		return true
	})
	return calls > 0 && inMemory
}

// packageWriters write to their first argument: io.WriteString(dst, s),
// fmt.Fprintf(dst, ...).
var packageWriters = map[string]bool{
	"io.WriteString": true, "io.Copy": true, "io.CopyN": true, "io.CopyBuffer": true,
	"fmt.Fprint": true, "fmt.Fprintf": true, "fmt.Fprintln": true,
}

// ioDestination returns what a read or write call reads from or writes to:
// the receiver of a method such as Write, or the first argument of a package
// writer such as fmt.Fprintf.
func ioDestination(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if packageWriters[typeName(sel)] {
		if len(call.Args) == 0 {
			return nil, false
		}
		return call.Args[0], true
	}
	if pkg, isPkg := sel.X.(*ast.Ident); isPkg && (pkg.Name == "io" || pkg.Name == "fmt") {
		return nil, false
	}
	return sel.X, ioMethods[sel.Sel.Name]
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && funcName(fd) == name {
			return fd
		}
	}
	return nil
}

func receiverOf(fd *ast.FuncDecl) (name, typeName string) {
	if fd.Recv == nil || len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 {
		return "", ""
	}
	return fd.Recv.List[0].Names[0].Name, strings.TrimPrefix(receiverType(fd.Recv.List[0].Type), "*")
}

// exprKey names a receiver the way localTypes and structFields index it:
// `w` or `e.w`. Anything more complex has no key and so no known type.
func exprKey(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name
		}
	case *ast.ParenExpr:
		return exprKey(x.X)
	case *ast.StarExpr:
		return exprKey(x.X)
	}
	return ""
}

// typeName renders a type expression as "pkg.Type", with pointers removed.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name + "." + t.Sel.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// localTypes maps parameters and locals to their declared types, for the
// declarations whose type is written out: `var b bytes.Buffer`, `b :=
// new(bytes.Buffer)`, `b := &bytes.Buffer{}`, and `b := bufio.NewWriter(w)`.
func localTypes(fd *ast.FuncDecl) map[string]string {
	types := map[string]string{}
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			types[name.Name] = typeName(field.Type)
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		declaredTypes(n, types)
		return true
	})
	return types
}

// declaredTypes records the types one declaration or short assignment writes
// out.
func declaredTypes(n ast.Node, types map[string]string) {
	switch d := n.(type) {
	case *ast.ValueSpec:
		if d.Type == nil {
			return
		}
		for _, name := range d.Names {
			types[name.Name] = typeName(d.Type)
		}
	case *ast.AssignStmt:
		if d.Tok != token.DEFINE || len(d.Lhs) != len(d.Rhs) {
			return
		}
		for i, lhs := range d.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				types[id.Name] = constructedType(d.Rhs[i])
			}
		}
	}
}

var constructors = map[string]string{
	"bufio.NewWriter": "bufio.Writer", "bufio.NewWriterSize": "bufio.Writer",
	"bufio.NewReader": "bufio.Reader", "bufio.NewReaderSize": "bufio.Reader", "bufio.NewScanner": "bufio.Scanner",
	"bytes.NewBuffer": "bytes.Buffer", "bytes.NewBufferString": "bytes.Buffer", "bytes.NewReader": "bytes.Reader",
	"strings.NewReader": "strings.Reader",
}

func constructedType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		if lit, ok := e.X.(*ast.CompositeLit); ok && e.Op == token.AND {
			return typeName(lit.Type)
		}
	case *ast.CompositeLit:
		return typeName(e.Type)
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" && len(e.Args) == 1 {
			return typeName(e.Args[0])
		}
		return constructors[typeName(e.Fun)]
	}
	return ""
}

// structFields reads the named struct's field types from the package
// directory, since the receiver's type is often declared in another file.
func structFields(dir, pkg, name string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil || f.Name.Name != pkg {
			continue
		}
		if fields := fieldsOf(f, name); fields != nil {
			return fields
		}
	}
	return nil
}

func fieldsOf(file *ast.File, name string) map[string]string {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
				return structFieldTypes(ts)
			}
		}
	}
	return nil
}

// structFieldTypes maps a struct type's named fields to their types; a type
// that is not a struct has none.
func structFieldTypes(ts *ast.TypeSpec) map[string]string {
	fields := map[string]string{}
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		return fields
	}
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			fields[n.Name] = typeName(field.Type)
		}
	}
	return fields
}
