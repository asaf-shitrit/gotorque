package campaign

import (
	"go/format"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// replaceIn writes src to a scratch main.go and replaces function f in it with
// newSrc, the way the function-source transport does.
func replaceIn(t *testing.T, src, newSrc string, imports ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	updated, err := replaceFunctionSource(path, []byte(src), "f", newSrc, imports)
	require.NoError(t, err)
	formatted, err := format.Source(updated)
	require.NoError(t, err)
	require.Equal(t, string(formatted), string(updated), "result is not gofmt-clean")
	return string(updated)
}

// The dasel candidate: fmt.Sprintf("%s/%s") became concatenation, fmt had no
// other use, and the build failed on the unused import.
func TestReplaceFunctionSourceDropsTheImportItOrphaned(t *testing.T) {
	src := `package execution

import (
	"context"
	"fmt"
)

func f(ctx context.Context, a, b string) string {
	return fmt.Sprintf("%s/%s", a, b)
}
`
	updated := replaceIn(t, src, "func f(ctx context.Context, a, b string) string {\n\treturn a + \"/\" + b\n}")
	require.Contains(t, updated, "import (\n\t\"context\"\n)")
	require.NotContains(t, updated, `"fmt"`)
}

// A remedy can swap one import for another: the added one is kept even though
// the original never used it, and the replaced one is dropped.
func TestReplaceFunctionSourceSwapsOneImportForAnother(t *testing.T) {
	src := `package main

import "fmt"

func f(n int) string {
	return fmt.Sprint(n)
}
`
	updated := replaceIn(t, src, "func f(n int) string {\n\treturn strconv.Itoa(n)\n}", "strconv")
	require.Contains(t, updated, "import (\n\t\"strconv\"\n)")
	require.NotContains(t, updated, `"fmt"`)
}

func TestReplaceFunctionSourceKeepsAnImportUsedElsewhere(t *testing.T) {
	src := `package main

import "fmt"

func f(a string) string {
	return fmt.Sprint(a)
}

func g() { fmt.Println() }
`
	updated := replaceIn(t, src, "func f(a string) string {\n\treturn a\n}")
	require.Contains(t, updated, `import "fmt"`)
}

func TestReplaceFunctionSourceRemovesALoneImportDeclaration(t *testing.T) {
	src := `package main

import "strconv"

func f(n int) string {
	return strconv.Itoa(n)
}
`
	updated := replaceIn(t, src, "func f(n int) string {\n\treturn string(rune('0' + n))\n}")
	require.NotContains(t, updated, "import")
	require.Contains(t, updated, "package main\n\nfunc f")
}

func TestReplaceFunctionSourceKeepsGroupsWhenDropping(t *testing.T) {
	src := `package main

import (
	"fmt"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

func f(s string) string {
	return fmt.Sprint(strings.ToUpper(s))
}

var _ = yaml.Marshal
`
	updated := replaceIn(t, src, "func f(s string) string {\n\treturn strings.ToUpper(s)\n}")
	require.Contains(t, updated, "import (\n\t\"strings\"\n\n\tyaml \"gopkg.in/yaml.v3\"\n)")
}

// A local that shadows a package name is resolved by the parser, so its
// disappearance never counts as the package's last use.
func TestReplaceFunctionSourceIgnoresAShadowingLocal(t *testing.T) {
	src := `package main

import "strings"

type box struct{ n int }

func f() int {
	strings := box{n: 1}
	return strings.n
}

var _ = strings.ToUpper
`
	updated := replaceIn(t, src, "func f() int {\n\treturn 1\n}")
	require.Contains(t, updated, `import "strings"`)
}

// An import the file used under a name the path does not spell is never
// matched, so it is kept rather than risk dropping a used one.
func TestReplaceFunctionSourceKeepsAnImportWhoseNameItCannotTell(t *testing.T) {
	src := `package main

import "example.com/go-thing"

func f() int {
	return thing.N
}
`
	updated := replaceIn(t, src, "func f() int {\n\treturn 1\n}")
	require.NotContains(t, updated, `"example.com/go-thing"`, "go- prefix is stripped, so thing is recognised")

	src2 := `package main

import "example.com/weirdpath"

func f() int {
	return other.N
}
`
	updated2 := replaceIn(t, src2, "func f() int {\n\treturn 1\n}")
	require.Contains(t, updated2, `import "example.com/weirdpath"`)
}

func TestImportNameSkipsBlankDotAndCgo(t *testing.T) {
	src := `package main

import (
	"C"
	. "math"
	_ "embed"
	"fmt"
)

func f() float64 {
	fmt.Println()
	return Pi
}
`
	updated := replaceIn(t, src, "func f() float64 {\n\treturn Pi\n}")
	require.Contains(t, updated, "\"C\"")
	require.Contains(t, updated, ". \"math\"")
	require.Contains(t, updated, "_ \"embed\"")
	require.NotContains(t, updated, "\"fmt\"")
}
