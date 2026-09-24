package campaign

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/jev"
)

// encoderSource mirrors gojq's encoder: an io.Writer it flushes to, and a
// *bytes.Buffer every write method appends to. The struct is declared in a
// file of its own, as a receiver's type often is.
const encoderTypes = `package cli

import (
	"bytes"
	"io"
)

type encoder struct {
	out io.Writer
	w   *bytes.Buffer
}
`

const encoderMethods = `package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

func (e *encoder) flush() error {
	_, err := e.out.Write(e.w.Bytes())
	e.w.Reset()
	return err
}

func (e *encoder) writeByte(b byte) {
	e.w.WriteByte(b)
}

func (e *encoder) writeQuoted(s string) {
	e.w.WriteByte('"')
	e.w.WriteString(s)
	e.w.WriteByte('"')
}

func report(w io.Writer, n int) {
	fmt.Fprintf(w, "%d\n", n)
}

func buffered(n int) {
	bw := bufio.NewWriter(os.Stdout)
	defer bw.Flush()
	fmt.Fprintln(bw, n)
	var sb strings.Builder
	sb.WriteString("x")
	io.WriteString(bw, sb.String())
}

func pure(a, b int) int { return a + b }
`

func encoderRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cli"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cli", "types.go"), []byte(encoderTypes), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cli", "encoder.go"), []byte(encoderMethods), 0o600))
	return repo
}

func TestOnlyInMemoryIO(t *testing.T) {
	repo := encoderRepo(t)
	for name, want := range map[string]bool{
		"(*encoder).writeByte":   true,  // e.w is a *bytes.Buffer
		"(*encoder).writeQuoted": true,  // every write goes to e.w
		"buffered":               true,  // bufio.Writer, strings.Builder, io.WriteString to the writer
		"(*encoder).flush":       false, // e.out is an io.Writer: a real write
		"report":                 false, // fmt.Fprintf to an io.Writer parameter
		"pure":                   false, // no I/O at all: nothing to judge
		"missing":                false,
	} {
		require.Equal(t, want, onlyInMemoryIO(repo, hotFunction{Path: "cli/encoder.go", Name: name}), name)
	}
}

func TestInMemoryWritersLoseTheUnbufferedFlag(t *testing.T) {
	repo := encoderRepo(t)
	flags := []jev.Score{{Cause: jev.CauseUnbufferedIO, Z: 2.89}, {Cause: jev.CauseRedundant, Z: 0.54}}

	writer := overruleInMemoryIO(repo, siteVerdict{Site: hotFunction{Path: "cli/encoder.go", Name: "(*encoder).writeByte"}, Flagged: flags})
	require.Equal(t, []jev.Score{{Cause: jev.CauseRedundant, Z: 0.54}}, writer.Flagged)
	require.Len(t, writer.Overruled, 1)
	require.Len(t, flags, 2, "the caller's scores are not modified")

	flusher := overruleInMemoryIO(repo, siteVerdict{Site: hotFunction{Path: "cli/encoder.go", Name: "(*encoder).flush"}, Flagged: flags})
	require.Equal(t, flags, flusher.Flagged)
	require.Empty(t, flusher.Overruled)
}
