package profile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func ownGron(symbol string) bool { return len(symbol) > 5 && symbol[:5] == "main." }

// A two-frame tree with self time at both levels: the parent's own weight is
// its count minus its child's.
const tinyCallGraph = `Call graph:
    10 Thread_1   DispatchQueue_1: com.apple.main-thread  (serial)
    + 10 main.run  (in target) + 12  [0x1]
    + ! 7 fmt.Fprintln  (in target) + 4  [0x2]
    + ! : 7 write  (in libsystem_kernel.dylib) + 8  [0x3]

Total number in stack (recursive counted multiple, when >=5):
`

func TestMacOSSampleStacksSplitsInclusiveCounts(t *testing.T) {
	stacks := MacOSSampleStacks(tinyCallGraph)
	want := []Stack{
		{Frames: []string{"write", "fmt.Fprintln", "main.run"}, Weight: 7},
		{Frames: []string{"main.run"}, Weight: 3},
	}
	if !slices.EqualFunc(stacks, want, func(a, b Stack) bool { return a.Weight == b.Weight && slices.Equal(a.Frames, b.Frames) }) {
		t.Fatalf("stacks = %+v, want %+v", stacks, want)
	}
}

func TestMacOSSampleStacksIgnoresReportsWithoutACallGraph(t *testing.T) {
	if stacks := MacOSSampleStacks("Sort by top of stack, same collapsed (when >= 5):\n        write  (in libsystem_kernel.dylib)        10\n"); len(stacks) != 0 {
		t.Errorf("stacks = %+v, want none", stacks)
	}
}

func TestAttributeToOwnCreditsTheInnermostOwnFrame(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"write", "fmt.Fprintln", "main.run", "main.main"}, Weight: 7},
		{Frames: []string{"main.run", "main.main"}, Weight: 3},
		{Frames: []string{"runtime.gcDrain", "runtime.gcBgMarkWorker"}, Weight: 50},
	}
	got := AttributeToOwn(stacks, ownGron)
	if len(got) != 1 || got[0].Name != "main.run" || got[0].Flat != "10" {
		t.Fatalf("attributed = %+v, want main.run with all 10 samples and no runtime frame", got)
	}
}

// TestAttributeToOwnSharesOrphanedSyscallsAmongObservedCallers covers the
// macOS unwinding gap: a Go system call switches to the system stack, and the
// sampler cannot walk back across it, so most write samples arrive with no Go
// caller at all. The few samples caught before the switch say who calls
// syscall.write, and the orphans are shared among those callers in proportion.
func TestAttributeToOwnSharesOrphanedSyscallsAmongObservedCallers(t *testing.T) {
	stacks := []Stack{
		{Frames: []string{"syscall.syscall", "syscall.write", "internal/poll.(*FD).Write", "fmt.Fprintln", "main.emit"}, Weight: 3},
		{Frames: []string{"syscall.write", "os.(*File).Write", "main.flush"}, Weight: 1},
		{Frames: []string{"write", "runtime.syscallN_trampoline.abi0", "runtime.asmcgocall.abi0"}, Weight: 100},
		// A wait with no observed Go wrapper stays unattributed.
		{Frames: []string{"__psynch_cvwait", "_pthread_cond_wait", "runtime.asmcgocall.abi0"}, Weight: 900},
	}
	got := map[string]string{}
	for _, fn := range AttributeToOwn(stacks, ownGron) {
		got[fn.Name] = fn.Flat
	}
	if got["main.emit"] != "78" || got["main.flush"] != "26" || len(got) != 2 {
		t.Fatalf("attributed = %v, want main.emit 3+75 and main.flush 1+25", got)
	}
}

// TestAttributeToOwnOnRealGronReport is the case that motivated attribution:
// gron's per-statement Fprintln loop, whose bufio fix was accepted at -11.8%,
// appeared in discovery only as standard-library frames. Credited to its own
// code, the loop is the hottest function in the thread.
func TestAttributeToOwnOnRealGronReport(t *testing.T) {
	report, err := os.ReadFile(filepath.Join("testdata", "macos-sample-gron-thread.txt"))
	if err != nil {
		t.Fatal(err)
	}
	attributed := AttributeToOwn(MacOSSampleStacks(string(report)), ownGron)
	if len(attributed) == 0 || attributed[0].Name != "main.gron" {
		t.Fatalf("attributed = %+v, want main.gron first", attributed)
	}
	if w := mustInt(t, attributed[0].Flat); w < 141 {
		t.Errorf("main.gron weight = %d, want at least the 141 orphaned write samples", w)
	}
}

func TestPerfScriptStacksKeepsEachEventLeafFirst(t *testing.T) {
	output := "gron 100 [000] 1.0: cycles:\n\t1 write (/usr/lib/libc.so)\n\t2 syscall.write (/tmp/gron)\n\t3 main.gron (/tmp/gron)\n\ngron 100 [000] 1.1: cycles:\n\t4 main.statements.Less (/tmp/gron)\n\t4 main.statements.Less (/tmp/gron)\n"
	stacks := PerfScriptStacks(output)
	want := []Stack{
		{Frames: []string{"write", "syscall.write", "main.gron"}, Weight: 1},
		{Frames: []string{"main.statements.Less"}, Weight: 1},
	}
	if !slices.EqualFunc(stacks, want, func(a, b Stack) bool { return a.Weight == b.Weight && slices.Equal(a.Frames, b.Frames) }) {
		t.Fatalf("stacks = %+v, want %+v", stacks, want)
	}
}

func TestSymbolPackage(t *testing.T) {
	for symbol, want := range map[string]string{
		"main.gron":                                     "main",
		"github.com/itchyny/gojq.(*env).Next":           "github.com/itchyny/gojq",
		"github.com/itchyny/gojq/cli.(*encoder).encode": "github.com/itchyny/gojq/cli",
		"internal/poll.(*FD).Write":                     "internal/poll",
		"write":                                         "",
		"github.com/a/b.c/d.F":                          "github.com/a/b.c/d",
	} {
		if got := SymbolPackage(symbol); got != want {
			t.Errorf("SymbolPackage(%q) = %q, want %q", symbol, got, want)
		}
	}
}
