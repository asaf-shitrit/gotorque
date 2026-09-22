package profile

import (
	"math"
	"regexp"
	"strings"
)

// Stack is one sampled call path, leaf first, with the samples it carried.
type Stack struct {
	Frames []string
	Weight int
}

// A call-graph row: the count's column gives the frame's depth. Thread
// headers sit at column 4 and every level indents two more columns, whatever
// mix of `+ ! : |` markers fills the indentation.
var macCallGraphRow = regexp.MustCompile(`^(\s*(?:[+!:|]\s*)*)(\d+)\s+(\S+)`)

type callNode struct {
	frame           string
	count, children int
}

// MacOSSampleStacks rebuilds weighted stacks from the call graph of a
// /usr/bin/sample report. Counts there are inclusive, so a node's own weight is
// its count minus its children's; every node with own weight yields one stack
// from that node up to its thread.
//
// The top-of-stack section ParseMacOSSample prefers says which frames were
// executing, but not on whose behalf: in a Go CLI that spends its time in fmt
// and write, the frames that carry the weight are all in the standard library,
// and the function that made those calls has almost no self time of its own.
func MacOSSampleStacks(report string) []Stack {
	start := strings.Index(report, "Call graph:")
	if start < 0 {
		return nil
	}
	var stacks []Stack
	var path []callNode
	pop := func(depth int) {
		for len(path) > depth {
			node := path[len(path)-1]
			if own := node.count - node.children; own > 0 {
				stacks = appendStack(stacks, path, own)
			}
			path = path[:len(path)-1]
		}
	}
	for _, line := range strings.Split(report[start:], "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		depth, node, ok := parseCallGraphRow(line)
		if !ok || depth > len(path) {
			continue
		}
		pop(depth)
		if depth > 0 {
			path[depth-1].children += node.count
		}
		path = append(path, node)
	}
	pop(0)
	return stacks
}

func parseCallGraphRow(line string) (int, callNode, bool) {
	match := macCallGraphRow.FindStringSubmatchIndex(line)
	if match == nil {
		return 0, callNode{}, false
	}
	column := match[4]
	if column < 4 || (column-4)%2 != 0 {
		return 0, callNode{}, false
	}
	count := atoi(line[match[4]:match[5]])
	return (column - 4) / 2, callNode{frame: macSampleSymbol(line[match[6]:match[7]]), count: count}, count > 0
}

// appendStack records the path, leaf first, dropping frames the symbol
// cleaner rejected (thread headers, process start-up).
func appendStack(stacks []Stack, path []callNode, weight int) []Stack {
	frames := make([]string, 0, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].frame != "" {
			frames = append(frames, path[i].frame)
		}
	}
	if len(frames) == 0 {
		return stacks
	}
	return append(stacks, Stack{Frames: frames, Weight: weight})
}

// PerfScriptStacks returns one stack per `perf script` event, leaf first, with
// consecutive duplicate frames (perf's recursion markers) counted once.
func PerfScriptStacks(output string) []Stack {
	var stacks []Stack
	var frames []string
	flush := func() {
		if len(frames) > 0 {
			stacks = append(stacks, Stack{Frames: frames, Weight: 1})
		}
		frames = nil
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || (!strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ")) {
			flush()
			continue
		}
		if name := perfScriptFrame(trimmed); name != "" && (len(frames) == 0 || frames[len(frames)-1] != name) {
			frames = append(frames, name)
		}
	}
	flush()
	return stacks
}

// AttributeToOwn credits every sample to the innermost frame own accepts: the
// target function on whose behalf the sample was spent, including the library
// and system calls it made. Samples with no own frame are dropped, except
// system calls orphaned from their Go caller (see orphanSyscall).
func AttributeToOwn(stacks []Stack, own func(string) bool) []Function {
	weights := map[string]float64{}
	callers := map[string]map[string]float64{}
	orphans := map[string]float64{}
	for _, s := range stacks {
		i := innermost(s.Frames, own)
		if i < 0 {
			if call, ok := orphanSyscall(s.Frames); ok {
				orphans[call] += float64(s.Weight)
			}
			continue
		}
		weights[s.Frames[i]] += float64(s.Weight)
		if call, ok := syscallBelow(s.Frames[:i]); ok {
			if callers[call] == nil {
				callers[call] = map[string]float64{}
			}
			callers[call][s.Frames[i]] += float64(s.Weight)
		}
	}
	shareOrphans(weights, orphans, callers)
	rounded := make(map[string]int, len(weights))
	for name, w := range weights {
		if n := int(math.Round(w)); n > 0 {
			rounded[name] = n
		}
	}
	return rankWeights(rounded)
}

func innermost(frames []string, own func(string) bool) int {
	for i, frame := range frames {
		if own(frame) {
			return i
		}
	}
	return -1
}

// syscallBelow finds the Go system-call wrapper (syscall.write) nearest the
// own frame, walking toward the leaf; its name matches the libc symbol the
// orphaned samples end in.
func syscallBelow(frames []string) (string, bool) {
	for i := len(frames) - 1; i >= 0; i-- {
		if call, ok := strings.CutPrefix(frames[i], "syscall."); ok && call != "" && call == strings.ToLower(call) && !strings.ContainsAny(call, ".(") {
			return call, true
		}
	}
	return "", false
}

// orphanSyscall recognizes a sample spent inside a libc system call whose Go
// caller the sampler could not see. A Go system call switches to the system
// stack through runtime.asmcgocall, and macOS sample cannot unwind back across
// that switch: on gron 141 of the samples spent in write sat under asmcgocall
// with no Go frame above them, while only three were caught before the switch
// with the full path from the output loop down to syscall.write.
func orphanSyscall(frames []string) (string, bool) {
	if len(frames) == 0 || strings.Contains(frames[0], ".") {
		return "", false
	}
	for _, frame := range frames[1:] {
		if strings.HasPrefix(frame, "runtime.asmcgocall") || strings.HasPrefix(frame, "runtime.syscall") {
			return frames[0], true
		}
	}
	return "", false
}

// shareOrphans divides each orphaned system call's samples among the own
// functions observed calling its Go wrapper, in proportion to how often each
// was seen doing so. The observed stacks are a sample of the same calls, so
// this is an estimate rather than a measurement; a call with no observed
// caller, such as a thread parked in __psynch_cvwait, stays unattributed.
func shareOrphans(weights, orphans map[string]float64, callers map[string]map[string]float64) {
	for call, orphaned := range orphans {
		var total float64
		for _, w := range callers[call] {
			total += w
		}
		if total == 0 {
			continue
		}
		for caller, w := range callers[call] {
			weights[caller] += orphaned * w / total
		}
	}
}

// SymbolPackage returns the import path of a Go symbol ("main" for
// main.gron, "github.com/itchyny/gojq" for github.com/itchyny/gojq.(*env).Next),
// or "" for a symbol with no package qualifier, such as a libc function.
func SymbolPackage(symbol string) string {
	if i := strings.Index(symbol, "["); i >= 0 {
		symbol = symbol[:i] // generic instantiations can contain slashes
	}
	slash := strings.LastIndex(symbol, "/")
	dot := strings.Index(symbol[slash+1:], ".")
	if dot < 0 {
		return ""
	}
	return symbol[:slash+1+dot]
}
