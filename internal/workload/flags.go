package workload

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Flag is one boolean command-line option a target's own source declares.
type Flag struct {
	// Name is the spelling passed on the command line: the longest alias,
	// with "--" for a long name and "-" for a single letter.
	Name string `json:"name"`
	// Aliases are the other spellings that set the same variable.
	Aliases []string `json:"aliases,omitempty"`
}

// BoolFlags lists the boolean flags declared by the Go files in dir, in the
// order they are declared. It reads the stdlib flag package and pflag/cobra
// calls (Bool, BoolVar, BoolP, BoolVarP on any receiver) and go-flags struct
// tags (`long:"stream"` on a bool field). Spellings that set the same variable
// are one flag: gron declares -s and --stream for one mode, and trying both
// would measure the same code path twice.
func BoolFlags(dir string) ([]Flag, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	c := collector{groups: map[string][]string{}}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(parsed, c.visit)
	}
	return c.flags(), nil
}

type collector struct {
	order  []string
	groups map[string][]string
}

func (c *collector) add(key string, names ...string) {
	if _, seen := c.groups[key]; !seen {
		c.order = append(c.order, key)
	}
	for _, n := range names {
		if n != "" {
			c.groups[key] = append(c.groups[key], n)
		}
	}
}

func (c *collector) visit(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.CallExpr:
		c.call(n)
	case *ast.Field:
		c.field(n)
	}
	return true
}

// boolCalls maps a flag constructor to the positions of its long name and,
// for the P forms, its shorthand; Var forms key aliases by their target.
var boolCalls = map[string]struct{ name, short int }{
	"Bool":     {name: 0, short: -1},
	"BoolP":    {name: 0, short: 1},
	"BoolVar":  {name: 1, short: -1},
	"BoolVarP": {name: 1, short: 2},
}

func (c *collector) call(call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	shape, ok := boolCalls[sel.Sel.Name]
	if !ok || len(call.Args) <= max(shape.name, shape.short) {
		return
	}
	name, ok := stringLiteral(call.Args[shape.name])
	if !ok {
		return
	}
	short := ""
	if shape.short >= 0 {
		short, _ = stringLiteral(call.Args[shape.short])
	}
	key := "name:" + name
	if strings.HasPrefix(sel.Sel.Name, "BoolVar") {
		key = "var:" + exprKey(call.Args[0])
	}
	c.add(key, name, short)
}

func (c *collector) field(f *ast.Field) {
	if f.Tag == nil || len(f.Names) == 0 {
		return
	}
	if ident, ok := f.Type.(*ast.Ident); !ok || ident.Name != "bool" {
		return
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return
	}
	long := reflect.StructTag(tag).Get("long")
	if long == "" {
		return
	}
	c.add("field:"+f.Names[0].Name, long, reflect.StructTag(tag).Get("short"))
}

func (c *collector) flags() []Flag {
	out := make([]Flag, 0, len(c.order))
	for _, key := range c.order {
		names := dedupe(c.groups[key])
		if len(names) == 0 {
			continue
		}
		sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		f := Flag{Name: spell(names[0])}
		for _, n := range names[1:] {
			f.Aliases = append(f.Aliases, spell(n))
		}
		out = append(out, f)
	}
	return out
}

func spell(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func dedupe(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil && s != ""
}

// exprKey renders the variable a Var form binds, such as &streamFlag or
// &opts.Stream, so aliases that share it fold together.
func exprKey(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		return e.Op.String() + exprKey(e.X)
	case *ast.SelectorExpr:
		return exprKey(e.X) + "." + e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return "?"
}
