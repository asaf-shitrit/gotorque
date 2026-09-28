package profile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func ownMain(frame string) bool { return strings.HasPrefix(frame, "main.") }

// TestUnbufferedWritesFindsARecordByRecordWriter: fzf's shape, a closure that
// prints each record, called from the loop over records.
func TestUnbufferedWritesFindsARecordByRecordWriter(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"syscall.write", "os.(*File).Write", "fmt.Fprintln", "main.defaultOptions.func1", "main.Run", "main.main"}, Weight: 90},
		{Frames: []string{"fmt.newPrinter", "fmt.Fprintln", "main.defaultOptions.func1", "main.Run"}, Weight: 5},
		{Frames: []string{"main.match", "main.Run"}, Weight: 100},
	}
	sites := UnbufferedWrites(stacks, ownMain, 0.5)
	require.Len(t, sites, 1)
	require.Equal(t, "main.defaultOptions.func1", sites[0].Function)
	require.InDelta(t, 90.0/95, sites[0].Share, 1e-9)
	require.Equal(t, []string{"main.Run"}, sites[0].Callers)
}

// TestUnbufferedWritesIgnoresBufferedOutput: the same volume of writes made
// through a bufio.Writer is already buffered.
func TestUnbufferedWritesIgnoresBufferedOutput(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"syscall.write", "os.(*File).Write", "bufio.(*Writer).Flush", "main.emit", "main.main"}, Weight: 90},
		{Frames: []string{"syscall.read", "os.(*File).Read", "main.load", "main.main"}, Weight: 90},
	}
	require.Empty(t, UnbufferedWrites(stacks, ownMain, 0.5))
}

// TestUnbufferedWritesSharesOrphanedWrites: most write samples on macOS sit
// under asmcgocall with no Go caller (gron: 141 orphaned, 3 attributed), and
// they count in proportion to each writer's observed writes.
func TestUnbufferedWritesSharesOrphanedWrites(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"syscall.write", "fmt.Fprintln", "main.gron", "main.main"}, Weight: 3},
		{Frames: []string{"syscall.write", "bufio.(*Writer).Flush", "main.report", "main.main"}, Weight: 1},
		{Frames: []string{"write", "runtime.asmcgocall"}, Weight: 140},
		{Frames: []string{"main.gron", "main.main"}, Weight: 20},
		{Frames: []string{"main.report", "main.main"}, Weight: 100},
	}
	sites := UnbufferedWrites(stacks, ownMain, 0.5)
	require.Len(t, sites, 1)
	require.Equal(t, "main.gron", sites[0].Function)
	// gron: 3 direct + 105 of the 140 orphans unbuffered, of 23 + 105 total.
	require.InDelta(t, 108.0/128, sites[0].Share, 1e-9)
	require.Equal(t, []string{"main.main"}, sites[0].Callers)
}

func TestUnbufferedWritesRanksByShareThenName(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"syscall.write", "main.b", "main.c"}, Weight: 10},
		{Frames: []string{"syscall.write", "main.a", "main.d"}, Weight: 10},
		{Frames: []string{"syscall.write", "main.a", "main.c"}, Weight: 10},
		{Frames: []string{"main.b"}, Weight: 5},
		{Frames: []string{"runtime.mallocgc"}, Weight: 50},
	}
	sites := UnbufferedWrites(stacks, ownMain, 0.5)
	require.Len(t, sites, 2)
	require.Equal(t, "main.a", sites[0].Function)
	require.Equal(t, []string{"main.c", "main.d"}, sites[0].Callers)
	require.Equal(t, "main.b", sites[1].Function)
	require.Empty(t, UnbufferedWrites(nil, ownMain, 0.5))
}
