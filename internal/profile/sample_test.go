package profile

import (
	"testing"
)

const macSampleReport = `Analysis of sampling gojq (pid 48213) every 1 millisecond
Call graph:
    2873 Thread_48213   DispatchQueue_1: com.apple.main-thread  (serial thread #1)
      2873 start
        2873 main
          2400 runQuery
            1900 parseInput
              1200 tokenize
                 600 normalize
                 ...
               700 compileQuery
      473 malloc
        473 free(void*)
    120 Thread_48214
      120 thread_start
        120 _pthread_start
Total number in stack: 2873

Sort by top of stack, same collapsed (when >= 5):
      1200  tokenize (in gojq)
       700  compileQuery (in gojq)
       473  free (in libsystem_malloc.dylib)
       120  __select (in libsystem_kernel.dylib)
`

func TestParseMacOSSamplePrefersTopOfStack(t *testing.T) {
	functions := ParseMacOSSample(macSampleReport)
	want := []struct{ name string; weight int }{
		{"tokenize", 1200},
		{"compileQuery", 700},
		{"free", 473},
		{"__select", 120},
	}
	if len(functions) != len(want) {
		t.Fatalf("function count = %d (%v)", len(functions), functions)
	}
	for i, w := range want {
		if functions[i].Name != w.name {
			t.Fatalf("functions[%d].Name = %q, want %q", i, functions[i].Name, w.name)
		}
		if functions[i].Flat != "1200" && i == 0 {
			t.Fatalf("functions[0].Flat = %q", functions[i].Flat)
		}
	}
}

func TestParseMacOSSampleFallsBackToCallGraph(t *testing.T) {
	report := `Analysis of sampling target (pid 100) every 1 millisecond
Call graph:
    100 Thread_100
      100 start
        100 main
          80 work
            60 crunch
Total number in stack: 100
`
	functions := ParseMacOSSample(report)
	var got []string
	for _, fn := range functions {
		got = append(got, fn.Name)
	}
	// Lifecycle noise like start/thread_start must be dropped; cumulative
	// call-graph counts rank frames hottest first.
	want := []string{"main", "work", "crunch"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

const perfScriptOutput = `gojq  48213/48213 [001] 12345.678901: cpu-clock:
        7ff6a100 main (+0x12ab) (/usr/local/bin/gojq)
        7ff6a200 runQuery (/usr/local/bin/gojq)
        7ff6a300 parseInput (/usr/local/bin/gojq)

gojq  48213/48213 [001] 12345.679912: cpu-clock:
        7ff6a100 main (+0x12ab) (/usr/local/bin/gojq)
        7ff6a300 parseInput (/usr/local/bin/gojq)
        7ff6a400 tokenize (/usr/local/bin/gojq)
        7ff6a500 [unknown] (/usr/lib/system/libdyld.dylib)

helper  991/991 [002] 12345.680001: cpu-clock:
        400000 _start
`

func TestParsePerfScriptCountsFramesPerEvent(t *testing.T) {
	functions := ParsePerfScript(perfScriptOutput)
	weights := map[string]int{}
	order := []string{}
	for _, fn := range functions {
		weights[fn.Name] = mustInt(t, fn.Flat)
		order = append(order, fn.Name)
	}
	if weights["parseInput"] != 2 {
		t.Fatalf("parseInput weight = %d, want 2", weights["parseInput"])
	}
	if weights["runQuery"] != 1 || weights["tokenize"] != 1 || weights["main"] != 2 {
		t.Fatalf("unexpected weights: %v (order %v)", weights, order)
	}
	if _, ok := weights["[unknown]"]; ok {
		t.Fatalf("[unknown] frames must be skipped")
	}
	// Hottest first with deterministic tie-break by name.
	if order[0] != "main" || order[1] != "parseInput" {
		t.Fatalf("order = %v, want main then parseInput first", order)
	}
}

func mustInt(t *testing.T, value string) int {
	t.Helper()
	n := 0
	for _, ch := range value {
		n = n*10 + int(ch-'0')
	}
	return n
}
