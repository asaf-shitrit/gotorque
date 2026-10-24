package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

const signaturesFixture = `package lib

type Record struct {
	Fields map[string]string
	order  []string
}

type unused struct{}

func encode(s string) string { return s }

func (r *Record) keys() []string { return r.order }

func (r *Record) Marshal() []byte {
	var out []byte
	for _, k := range r.keys() {
		out = append(out, encode(r.Fields[k])...)
	}
	return out
}

func other() {}
`

func TestTargetSignaturesListsWhatTheTargetUses(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "lib"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib", "lib.go"), []byte(signaturesFixture), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib", "lib_test.go"), []byte("package lib\n\nfunc encode2() {}\n"), 0o600))

	got := targetSignatures(repo, agents.Target{Function: "(*Record).Marshal", Location: "lib/lib.go:15"})
	require.Len(t, got, 3, "%q", got)
	require.True(t, strings.HasPrefix(got[0], "type Record struct"), got[0])
	require.Contains(t, got[0], "Fields map[string]string")
	require.Equal(t, "func (r *Record) keys() []string", got[1])
	require.Equal(t, "func encode(s string) string", got[2])
	for _, g := range got {
		require.NotContains(t, g, "unused")
		require.NotContains(t, g, "other")
		require.NotContains(t, g, "Marshal")
	}
}

func TestTargetSignaturesDegradesQuietly(t *testing.T) {
	require.Nil(t, targetSignatures(t.TempDir(), agents.Target{}))
	require.Nil(t, targetSignatures(t.TempDir(), agents.Target{Function: "f", Location: "missing/x.go:1"}))
}

func TestCutLines(t *testing.T) {
	require.Equal(t, "a\nb", cutLines("a\nb", 2))
	require.Equal(t, "a\nb\n\t// ... cut", cutLines("a\nb\nc", 2))
}

// TestTargetSignaturesOnMiller checks the case that motivated it against the
// real clone, when present: marshalJSONAuxMultiline's context must carry the
// declaration the optimizer got wrong twice.
func TestTargetSignaturesOnMiller(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	repo := filepath.Join(home, "projects", "gotorque-work", "miller")
	if _, err := os.Stat(filepath.Join(repo, "pkg", "mlrval", "mlrmap_json.go")); err != nil {
		t.Skip("miller clone not present")
	}
	got := targetSignatures(repo, agents.Target{Function: "(*Mlrmap).marshalJSONAuxMultiline", Location: "pkg/mlrval/mlrmap_json.go:57"})
	require.NotEmpty(t, got)
	require.LessOrEqual(t, len(got), maxSignatures)
	t.Logf("context:\n%s", strings.Join(got, "\n---\n"))
}

func TestTargetSignaturesFollowsFirstPartyPackages(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(src), 0o600))
	}
	write("go.mod", "module example.com/m\n\ngo 1.26\n")
	write("types/types.go", "package types\n\ntype Context struct{ N int }\n\ntype RecordAndContext struct {\n\tRecord  string\n\tContext Context\n}\n\nfunc New(r string, c *Context) *RecordAndContext { return nil }\n")
	write("input/reader.go", "package input\n\nimport (\n\t\"strings\"\n\n\t\"example.com/m/types\"\n)\n\nfunc read(c *types.Context) *types.RecordAndContext {\n\treturn types.New(strings.ToUpper(\"x\"), c)\n}\n")

	got := targetSignatures(repo, agents.Target{Function: "read", Location: "input/reader.go:9"})
	joined := strings.Join(got, "\n")
	require.Contains(t, joined, "// package types\ntype Context struct")
	require.Contains(t, joined, "type RecordAndContext struct")
	require.Contains(t, joined, "func New(r string, c *Context) *RecordAndContext")
	require.NotContains(t, joined, "ToUpper")
}

func TestModulePathOf(t *testing.T) {
	require.Equal(t, "example.com/m", modulePathOf([]byte("// c\nmodule example.com/m\n\ngo 1.26\n")))
	require.Equal(t, "example.com/q", modulePathOf([]byte("module \"example.com/q\"\n")))
	require.Empty(t, modulePathOf([]byte("go 1.26\n")))
	root, module := moduleOf(t.TempDir())
	require.Empty(t, root)
	require.Empty(t, module)
}

func TestTargetSignaturesOnMillerCrossesIntoPkgTypes(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	repo := filepath.Join(home, "projects", "gotorque-work", "miller")
	if _, err := os.Stat(filepath.Join(repo, "pkg", "input", "record_reader_csv.go")); err != nil {
		t.Skip("miller clone not present")
	}
	got := targetSignatures(repo, agents.Target{Function: "(*RecordReaderCSV).getRecordBatch", Location: "pkg/input/record_reader_csv.go:260"})
	require.Contains(t, strings.Join(got, "\n"), "// package types")
	t.Logf("context:\n%s", strings.Join(got, "\n---\n"))
}

// TestTargetSignaturesShowsOneVariantOfABuildTaggedDeclaration is
// klauspost/compress's internal/race: WriteSlice and ReadSlice are declared
// in a race and a !race file. Only the variant this platform builds is
// shown, so both functions the target calls fit the context.
func TestTargetSignaturesShowsOneVariantOfABuildTaggedDeclaration(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(src), 0o600))
	}
	write("go.mod", "module example.com/c\n\ngo 1.26\n")
	write("internal/race/race.go", "//go:build race\n\npackage race\n\n// WriteSlice is the race variant.\nfunc WriteSlice[T any](s []T) {}\n\n// ReadSlice is the race variant.\nfunc ReadSlice[T any](s []T) {}\n")
	write("internal/race/norace.go", "//go:build !race\n\npackage race\n\nfunc WriteSlice[T any](s []T) {}\n\nfunc ReadSlice[T any](s []T) {}\n")
	write("s2/writer.go", "package s2\n\nimport \"example.com/c/internal/race\"\n\nfunc encode(in, out []byte) {\n\trace.WriteSlice(out)\n\trace.ReadSlice(in)\n}\n")

	joined := strings.Join(targetSignatures(repo, agents.Target{Function: "encode", Location: "s2/writer.go:5"}), "\n")
	require.Equal(t, 1, strings.Count(joined, "func WriteSlice"), joined)
	require.Equal(t, 1, strings.Count(joined, "func ReadSlice"), joined)
	require.NotContains(t, joined, "race variant", "the race-tagged file is not built by default")
}

// valueTypesFixture is the jj case the issue reports: main's only context
// entry was `func parseArgs() args`, so the candidate invented fields on
// args. Every way a value gets a first-party type appears once here.
const valueTypesFixture = `package main

type args struct {
	sort   bool
	indent int
}

func (a args) apply() {}

func parseArgs() args { return args{} }

func newArgs() *args { return nil }

func run(flags args, src string) {
	opts := parseArgs()
	opts.apply()
	flags.apply()
	other := newArgs()
	other.apply()
	var third args
	third.apply()
	converted := args(flags)
	converted.apply()
	src.Frame()
}
`

func TestTargetSignaturesCarriesTheTypesOfItsValues(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cmd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cmd", "main.go"), []byte(valueTypesFixture), 0o600))

	got := targetSignatures(repo, agents.Target{Function: "run", Location: "cmd/main.go:17"})
	joined := strings.Join(got, "\n---\n")
	require.Contains(t, joined, "func parseArgs() args")
	require.Contains(t, joined, "type args struct")
	require.Contains(t, joined, "sort   bool")
	require.Equal(t, 1, strings.Count(joined, "func (a args) apply()"), joined)
	require.Contains(t, joined, "func newArgs() *args")
}

// TestTargetSignaturesCarriesFirstPartyValueTypes is miller's shape: a value
// whose type another package of the module declares.
func TestTargetSignaturesCarriesFirstPartyValueTypes(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(src), 0o600))
	}
	write("go.mod", "module example.com/m\n\ngo 1.26\n")
	write("types/types.go", "package types\n\ntype Record struct {\n\tName string\n}\n\nfunc New() *Record { return nil }\n\nfunc (r *Record) Encode() string { return r.Name }\n")
	write("input/reader.go", "package input\n\nimport \"example.com/m/types\"\n\nfunc read() string {\n\trec := types.New()\n\treturn rec.Encode()\n}\n")

	joined := strings.Join(targetSignatures(repo, agents.Target{Function: "read", Location: "input/reader.go:5"}), "\n---\n")
	require.Contains(t, joined, "// package types\nfunc New() *Record")
	require.Contains(t, joined, "// package types\ntype Record struct")
	require.Contains(t, joined, "// package types\nfunc (r *Record) Encode() string")
}

// TestTargetSignaturesIgnoresValuesItCannotType keeps a value of an unknown or
// unresolved type from adding anything: the pass only ever adds advice.
func TestTargetSignaturesIgnoresValuesItCannotType(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "lib"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib", "lib.go"), []byte("package lib\n\nfunc run(s string, m map[string]int, any0 any) {\n\ts.Len()\n\tm.Get(\"k\")\n\tany0.Thing()\n\tunknown()\n}\n"), 0o600))

	require.Empty(t, targetSignatures(repo, agents.Target{Function: "run", Location: "lib/lib.go:3"}))
}

func TestBaseTypeName(t *testing.T) {
	fset := token.NewFileSet()
	expr := func(src string) ast.Expr {
		e, err := parser.ParseExprFrom(fset, "", src, parser.SkipObjectResolution)
		require.NoError(t, err)
		return e
	}
	require.Equal(t, "Record", baseTypeName(expr("Record")))
	require.Equal(t, "Record", baseTypeName(expr("*Record")))
	require.Equal(t, "Record", baseTypeName(expr("Record[T]")))
	require.Equal(t, "Record", baseTypeName(expr("Record[T, U]")))
	require.Empty(t, baseTypeName(expr("[]Record")))
}
