package profile

import "testing"

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	return root
}

func TestParseSymbol(t *testing.T) {
	tests := map[string]Symbol{
		"go.starlark.net/starlark.(*Function).CallInternal": {"go.starlark.net/starlark", "Function", "CallInternal"},
		"go.starlark.net/starlark.Int.get":                  {"go.starlark.net/starlark", "Int", "get"},
		"go.starlark.net/starlark.Binary":                   {"go.starlark.net/starlark", "", "Binary"},
		"main.main":                                         {"main", "", "main"},
		"github.com/x/y.(*Map[...]).Get":                    {"github.com/x/y", "Map", "Get"},
		"github.com/x/y.Sum[...]":                           {"github.com/x/y", "", "Sum"},
		"github.com/x/y.T.m-fm":                             {"github.com/x/y", "T", "m"},
		"bare":                                              {"", "", "bare"},
	}
	for name, want := range tests {
		if got := ParseSymbol(name); got != want {
			t.Errorf("ParseSymbol(%q) = %+v, want %+v", name, got, want)
		}
	}
}

// starlarkLike reproduces the live1-starlark misresolution: the same method
// name on several receivers, in several packages, and in a test file that
// sorts first.
func starlarkLike(t *testing.T) string {
	t.Helper()
	return tree(t, map[string]string{
		"go.mod":                    "module go.starlark.net\n",
		"lib/proto/proto.go":        "package proto\n\ntype Field struct{}\n\nfunc (f Field) get() int { return 0 }\n",
		"starlark/example_test.go":  "package starlark_test\n\nfunc Binary() {}\n",
		"starlark/eval.go":          "package starlark\n\ntype Function struct{}\n\nfunc (fn *Function) CallInternal() {}\n\nfunc Binary() {}\n",
		"starlark/int.go":           "package starlark\n\ntype Int struct{}\ntype big struct{}\n\nfunc (b big) get() int { return 1 }\n\nfunc (i Int) get() int {\n\treturn 2\n}\n",
		"starlark/int_generic.go":   "package starlark\n\ntype Set[T any] struct{}\n\nfunc (s *Set[T]) Len() int { return 0 }\n",
		"starlark/a_other.go":       "//go:build ignore\n\npackage starlark\n\nfunc (i Int) get() int { return 3 }\n",
		"vendor/v/v.go":             "package v\n\nfunc Stolen() {}\n",
		"cmd/a/main.go":             "package main\n\nfunc run() {}\n",
		"cmd/b/main.go":             "package main\n\nfunc run() {}\n",
		"cmd/b/lib/lib.go":          "package lib\n\nfunc only() {}\n",
		"internal/compile/emit.go":  "package compile\n\nfunc Emit[T any](v T) {}\n",
		"internal/compile/other.go": "package compile\n",
	})
}

func TestFindDeclarationQualifiesByPackageAndReceiver(t *testing.T) {
	root := starlarkLike(t)
	tests := []struct {
		name, path string
		line       int
	}{
		{"go.starlark.net/starlark.(*Function).CallInternal", "starlark/eval.go", 5},
		{"go.starlark.net/starlark.Binary", "starlark/eval.go", 7},
		{"go.starlark.net/starlark.Int.get", "starlark/int.go", 8},
		{"go.starlark.net/starlark.(*Set[...]).Len", "starlark/int_generic.go", 5},
		{"go.starlark.net/internal/compile.Emit[...]", "internal/compile/emit.go", 3},
		{"go.starlark.net/lib/proto.Field.get", "lib/proto/proto.go", 5},
	}
	for _, tt := range tests {
		path, line, ok := FindDeclaration(root, ParseSymbol(tt.name))
		if !ok || path != tt.path || line != tt.line {
			t.Errorf("%s: got %q:%d ok=%v, want %s:%d", tt.name, path, line, ok, tt.path, tt.line)
		}
	}
}

func TestFindDeclarationRefusesWrongOrAmbiguousMatches(t *testing.T) {
	root := starlarkLike(t)
	for _, name := range []string{
		"go.starlark.net/starlark.String.get", // no such receiver in the package
		"go.starlark.net/starlark.get",        // methods are not functions
		"main.run",                            // two commands declare it
		"other.module/pkg.Stolen",             // vendored code is skipped
		"go.starlark.net/starlark.Missing",
		"",
	} {
		if path, line, ok := FindDeclaration(root, ParseSymbol(name)); ok {
			t.Errorf("%q resolved to %s:%d, want no location", name, path, line)
		}
	}
}

func TestFindDeclarationSearchesTheRepoOutsideTheRootModule(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":             "package main\n\nfunc main() {}\n",
		"lib/lib.go":          "package lib\n\nfunc main() {}\n",
		"lib/lib_test.go":     "package lib\n\nfunc Unique() {}\n",
		"internal/x/x.go":     "package x\n\ntype scanner struct{}\n\nfunc (s *scanner) Scan() int {\n\treturn 1\n}\n",
		"testdata/fixture.go": "package fixture\n\nfunc Scan() {}\n",
		"readme.txt":          "func Unique() {}\n",
	})
	if path, line, ok := FindDeclaration(root, ParseSymbol("main.main")); !ok || path != "main.go" || line != 3 {
		t.Errorf("main.main: got %q:%d ok=%v, want main.go:3 (only package main counts)", path, line, ok)
	}
	if path, line, ok := FindDeclaration(root, ParseSymbol("example.com/m/internal/x.(*scanner).Scan")); !ok || path != "internal/x/x.go" || line != 5 {
		t.Errorf("Scan: got %q:%d ok=%v, want internal/x/x.go:5", path, line, ok)
	}
	if path, _, ok := FindDeclaration(root, ParseSymbol("example.com/m/lib.Unique")); ok {
		t.Errorf("Unique resolved to %s; test files and non-Go files must be skipped", path)
	}
}
