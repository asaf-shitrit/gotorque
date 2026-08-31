package profile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSampleTargetProfileRequiresBinaryPath(t *testing.T) {
	_, err := SampleTargetProfile(context.Background(), SampleTarget{OutputPath: filepath.Join(t.TempDir(), "out.txt")})
	if err == nil {
		t.Fatal("expected error for missing binary path")
	}
}

func TestSampleTargetProfileRequiresAbsoluteBinaryPath(t *testing.T) {
	_, err := SampleTargetProfile(context.Background(), SampleTarget{BinaryPath: "relative/bin", OutputPath: filepath.Join(t.TempDir(), "out.txt")})
	if err == nil {
		t.Fatal("expected error for non-absolute binary path")
	}
}

func TestSampleTargetProfileRequiresAbsoluteOutputPath(t *testing.T) {
	_, err := SampleTargetProfile(context.Background(), SampleTarget{BinaryPath: "/bin/true", OutputPath: "relative.txt"})
	if err == nil {
		t.Fatal("expected error for non-absolute output path")
	}
	_, err = SampleTargetProfile(context.Background(), SampleTarget{BinaryPath: "/bin/true"})
	if err == nil {
		t.Fatal("expected error for empty output path")
	}
}

func TestSampleMacOSSamplerUnavailable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sampler path")
	}
	req := SampleTarget{
		BinaryPath:   "/usr/bin/true",
		OutputPath:   filepath.Join(t.TempDir(), "out.txt"),
		Duration:     time.Second,
		SampleBinary: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	_, err := SampleTargetProfile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when sample tool is unavailable")
	}
}

func TestSampleMacOSTargetExitsBeforeSamplingBegins(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sampler path")
	}
	req := SampleTarget{
		BinaryPath:   "/usr/bin/true",
		OutputPath:   filepath.Join(t.TempDir(), "out.txt"),
		Duration:     time.Second,
		SampleBinary: writeFakeSampler(t, 0, "unused"),
	}
	_, err := SampleTargetProfile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for target that exits immediately")
	}
}

func TestSampleMacOSSamplerFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sampler path")
	}
	req := SampleTarget{
		BinaryPath:   "/bin/sleep",
		Args:         []string{"2"},
		OutputPath:   filepath.Join(t.TempDir(), "out.txt"),
		Duration:     time.Second,
		SampleBinary: writeFakeSampler(t, 1, "boom"),
	}
	_, err := SampleTargetProfile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when sampler command exits nonzero")
	}
}

func TestSampleMacOSHappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sampler path")
	}
	outputPath := filepath.Join(t.TempDir(), "nested", "out.txt")
	req := SampleTarget{
		BinaryPath:   "/bin/sleep",
		Args:         []string{"2"},
		OutputPath:   outputPath,
		Duration:     time.Second,
		SampleBinary: writeFakeSampler(t, 0, macSampleReport),
	}
	result, err := SampleTargetProfile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Sampler != "macos-sample" {
		t.Fatalf("Sampler = %q, want macos-sample", result.Sampler)
	}
	if len(result.Functions) == 0 {
		t.Fatal("expected parsed functions")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected raw report written: %v", err)
	}
}

// writeFakeSampler creates an executable script that mimics
// `/usr/bin/sample <pid> <duration> -file <path>` by writing report to the
// path named by its fourth argument and exiting with the given status.
func writeFakeSampler(t *testing.T, exitCode int, report string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-sample.sh")
	script := "#!/bin/sh\n" +
		"OUT=\"$4\"\n" +
		"if [ -n \"$OUT\" ]; then printf '%s' " + shellQuote(report) + " > \"$OUT\"; fi\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestPrepareWorkDirRejectsRelativeTempRoot(t *testing.T) {
	if _, _, err := prepareWorkDir(SampleTarget{TempRoot: "relative"}); err == nil {
		t.Fatal("expected error for relative temp root")
	}
}

func TestPrepareWorkDirMaterializesFixtures(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := prepareWorkDir(SampleTarget{TempRoot: root, Fixtures: map[string][]byte{
		"a/b.txt":  []byte("hello"),
		"../c.txt": []byte("escaped"),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(dir, "a", "b.txt"))
	if err != nil {
		t.Fatalf("expected fixture written: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q, want hello", data)
	}
	// A path-escaping fixture must be cleaned to stay inside dir.
	if _, err := os.ReadFile(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatalf("expected escaped fixture confined to workdir: %v", err)
	}
}

func TestRunBoundedCapturesOutput(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf hello; exit 3")
	output, err := runBounded(cmd)
	if string(output) != "hello" {
		t.Fatalf("output = %q, want hello", output)
	}
	if err == nil {
		t.Fatal("expected non-nil error for nonzero exit")
	}
}

func TestLimitedWriterRejectsOversizedWrite(t *testing.T) {
	w := &limitedWriter{}
	oversized := make([]byte, maxSamplerOutputBytes+1)
	if _, err := w.Write(oversized); err == nil {
		t.Fatal("expected error for output exceeding limit")
	}
	if !w.exceeded {
		t.Fatal("expected exceeded flag set")
	}
}

func TestLimitedWriterAllowsWithinLimit(t *testing.T) {
	w := &limitedWriter{}
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if w.exceeded {
		t.Fatal("did not expect exceeded flag")
	}
}

func TestTerminateNilProcess(t *testing.T) {
	if err := terminate(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTerminateStopsRunningProcess(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := terminate(cmd.Process); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("terminate took too long: %v", elapsed)
	}
}

func TestFinishSampleResultRejectsEmptyOutput(t *testing.T) {
	if _, err := finishSampleResult("macos-sample", filepath.Join(t.TempDir(), "out.txt"), "   "); err == nil {
		t.Fatal("expected error for empty sampler output")
	}
}

func TestFinishSampleResultRejectsNoRecognizableFrames(t *testing.T) {
	if _, err := finishSampleResult("linux-perf", filepath.Join(t.TempDir(), "out.txt"), "no frames here\n"); err == nil {
		t.Fatal("expected error when no frames are recognizable")
	}
}

func TestFinishSampleResultLinuxPerfHappyPath(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "nested", "out.txt")
	result, err := finishSampleResult("linux-perf", outputPath, perfScriptOutput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Sampler != "linux-perf" {
		t.Fatalf("Sampler = %q, want linux-perf", result.Sampler)
	}
	if len(result.Functions) == 0 {
		t.Fatal("expected parsed functions")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected raw report written: %v", err)
	}
}

func TestTruncateForError(t *testing.T) {
	short := truncateForError([]byte("  short  "))
	if short != "short" {
		t.Fatalf("short = %q, want short", short)
	}
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateForError(long)
	if len(got) != 515 { // 512 bytes + 3-byte "…" ellipsis
		t.Fatalf("truncated length = %d", len(got))
	}
}

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
	want := []struct {
		name   string
		weight int
	}{
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
