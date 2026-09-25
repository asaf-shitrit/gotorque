package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

func funcNamed(t *testing.T, src, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "a.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	fd := findFunc(file, name)
	require.NotNil(t, fd)
	return fd
}

// TestGrowsInLoopFindsWhatAPreallocationChanges: an append or map write in a
// loop to a container code cannot see sized is growth; one made with a length
// or capacity, or growth outside any loop, is not.
func TestGrowsInLoopFindsWhatAPreallocationChanges(t *testing.T) {
	src := `package a

func unsized(xs []int) []int {
	var out []int
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

func zeroLength(xs []int) []int {
	out := make([]int, 0)
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

func sized(xs []int) []int {
	out := make([]int, 0, len(xs))
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

func mapLiteral(xs []string) map[string]bool {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return seen
}

func sizedMap(xs []string) map[string]bool {
	seen := make(map[string]bool, len(xs))
	for _, x := range xs {
		seen[x] = true
	}
	return seen
}

func outsideLoop(x int) []int {
	var out []int
	out = append(out, x)
	return out
}
`
	for name, want := range map[string]bool{
		"unsized": true, "zeroLength": true, "mapLiteral": true,
		"sized": false, "sizedMap": false, "outsideLoop": false,
	} {
		require.Equal(t, want, growsInLoop(funcNamed(t, src, name), nil), name)
	}
}

// TestHasRealIOSeesPrintsAndUnresolvedWriters: a print to the process's own
// streams and a write whose destination code cannot resolve count as real I/O;
// a function with no I/O call has nothing a buffering remedy could batch.
func TestHasRealIOSeesPrintsAndUnresolvedWriters(t *testing.T) {
	src := `package a

import (
	"fmt"
	"io"
	"strings"
)

func prints(xs []string) {
	for _, x := range xs {
		fmt.Println(x)
	}
}

func writes(w io.Writer, b []byte) {
	w.Write(b)
}

func builds(xs []string) string {
	var sb strings.Builder
	for _, x := range xs {
		sb.WriteString(x)
	}
	return sb.String()
}

func pure(a, b int) int { return a + b }
`
	for name, want := range map[string]bool{"prints": true, "writes": true, "builds": false, "pure": false} {
		fd := funcNamed(t, src, name)
		require.Equal(t, want, hasRealIO(fd, localTypes(fd)), name)
	}
}
