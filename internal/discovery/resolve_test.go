package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(src), 0o600))
	}
}

// TestResolveFoldsSymbolsSharingADeclaration: the value method and Go's
// generated pointer wrapper resolve to one declaration and fill one slot.
func TestResolveFoldsSymbolsSharingADeclaration(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{"statements.go": "package main\n\ntype statements []string\n\nfunc (ss statements) Less(a, b int) bool {\n\treturn ss[a] < ss[b]\n}\n"})
	got := locator{repository: repo}.resolve(context.Background(), "", []string{"main.statements.Less", "main.(*statements).Less", "strings.Join"})
	require.Equal(t, []string{"statements.go:5", "strings.Join"}, got)
}

// TestResolveQualifiesMethodsByReceiverAndPackage reproduces live1-starlark:
// bare-name search sent (*Function).CallInternal to another package's
// CallInternal, Binary to a method named Binary, and Int.get into a test file.
func TestResolveQualifiesMethodsByReceiverAndPackage(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod":                   "module go.starlark.net\n",
		"lib/proto/proto.go":       "package proto\n\ntype D struct{}\n\nfunc (d D) CallInternal() {}\n",
		"lib/time/time.go":         "package time\n\ntype Duration int\n\nfunc (d Duration) Binary() {}\n",
		"starlark/example_test.go": "package starlark_test\n\ntype cache struct{}\n\nfunc (c *cache) get() {}\n",
		"starlark/eval.go":         "package starlark\n\ntype Function struct{}\n\nfunc (fn *Function) CallInternal() {}\n\nfunc Binary() {}\n",
		"starlark/int.go":          "package starlark\n\ntype Int struct{}\n\nfunc (i Int) get() {}\n",
	})
	got := locator{repository: repo}.resolve(context.Background(), "", []string{
		"go.starlark.net/starlark.(*Function).CallInternal",
		"go.starlark.net/starlark.Binary",
		"go.starlark.net/starlark.Int.get",
		"go.starlark.net/starlark.(*Function).CallInternal.func1",
		"go.starlark.net/starlark.Int.get (inline)",
	})
	require.Equal(t, []string{"starlark/eval.go:5", "starlark/eval.go:7", "starlark/int.go:5"}, got, "an (inline) frame folds into its declaration")
}

// TestResolveFindsMainInTheBuiltCommand: two commands declare run, and a
// sampled main.run belongs to the one the manifest builds.
func TestResolveFindsMainInTheBuiltCommand(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod":        "module example.com/tool\n",
		"cmd/a/main.go": "package main\n\nfunc run() {}\n",
		"cmd/b/main.go": "package main\n\nfunc main() {}\n\nfunc run() {}\n",
	})
	loc := locator{repository: repo, build: manifest.BuildTarget{Package: "./cmd/b"}}
	require.Equal(t, []string{"cmd/b/main.go:5"}, loc.resolve(context.Background(), "", []string{"main.run"}))

	loc.build.Package = "example.com/tool/cmd/b"
	require.Equal(t, []string{"main.run"}, loc.resolve(context.Background(), "", []string{"main.run"}), "an import-path package names no directory to prefer, and run is ambiguous")
}

func TestResolveStopsAtTheBudget(t *testing.T) {
	names := make([]string, 0, 2*hotFunctionBudget)
	for i := range 2 * hotFunctionBudget {
		names = append(names, "pkg.F"+string(rune('a'+i)))
	}
	require.Len(t, locator{repository: t.TempDir()}.resolve(context.Background(), "", names), hotFunctionBudget)
}

// TestResolveSkipsTestFilesInTheProfile profiles a benchmark that spends its
// time in a helper declared in a _test.go file, as scc's filereader_test.go
// did. The helper stays a bare name, since no patch may edit a test file; the
// library function resolves to its line through pprof -list.
func TestResolveSkipsTestFilesInTheProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and profiles a benchmark")
	}
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod":      "module example.com/hot\n\ngo 1.22\n",
		"lib.go":      "package hot\n\nfunc Work(n int) int {\n\ts := 0\n\tfor i := range n {\n\t\ts += i * i % 7\n\t}\n\treturn s\n}\n",
		"lib_test.go": "package hot\n\nimport \"testing\"\n\nfunc helper(n int) int {\n\ts := 0\n\tfor i := range n {\n\t\ts ^= i * 31 % 11\n\t}\n\treturn s\n}\n\nvar sink int\n\nfunc BenchmarkHot(b *testing.B) {\n\tfor range b.N {\n\t\tsink += Work(3e7) + helper(3e7)\n\t}\n}\n",
	})
	prof := filepath.Join(t.TempDir(), "cpu.pb.gz")
	tc := toolchain.New(toolchain.Options{})
	_, err := tc.Test(context.Background(), toolchain.TestRequest{Repository: repo, Bench: "Hot", Count: 1, Cpuprofile: prof, Output: filepath.Join(t.TempDir(), "hot.test"), Env: []string{"GOFLAGS=-benchtime=3x"}})
	require.NoError(t, err)
	got := locator{repository: repo, tc: tc}.resolve(context.Background(), prof, []string{"example.com/hot.Work", "example.com/hot.helper (inline)"})
	require.Equal(t, []string{"lib.go:3", "example.com/hot.helper"}, got)
}
