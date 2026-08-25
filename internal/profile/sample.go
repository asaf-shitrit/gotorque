package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SampleTarget profiles an already-built target binary directly, without
// requiring Go test benchmarks, so every target can produce hot-path data.
//
// Platform strategy (strictly best-effort; callers must treat errors as a
// reason to skip discovery evidence):
//   - macOS: start the binary, then attach /usr/bin/sample <pid> <duration>
//     -file <report>, which produces a call-graph text report.
//   - Linux: run `perf record` around the binary and convert the result with
//     `perf script`. Sandboxes and containers frequently lack perf; any
//     failure is reported so the caller can skip cleanly.
//
// The returned Functions use the same shape as pprof summaries: Flat holds
// the observed sample weight (a count for sample reports) and Name holds the
// function symbol, ordered hottest first.
type SampleTarget struct {
	BinaryPath string
	Args       []string
	Stdin      []byte
	Fixtures   map[string][]byte
	Duration   time.Duration
	// OutputPath receives the raw sampler report text.
	OutputPath string
	// TempRoot is an optional directory for scratch fixture materialization;
	// it defaults to os.TempDir().
	TempRoot string

	// SampleBinary overrides /usr/bin/sample (tests only).
	SampleBinary string
}

type SampleResult struct {
	Sampler   string
	Functions []Function
	RawReport string
}

func SampleTargetProfile(ctx context.Context, req SampleTarget) (SampleResult, error) {
	if req.BinaryPath == "" {
		return SampleResult{}, errors.New("binary path is required")
	}
	if !filepath.IsAbs(req.BinaryPath) {
		return SampleResult{}, errors.New("binary path must be absolute")
	}
	if req.OutputPath == "" || !filepath.IsAbs(req.OutputPath) {
		return SampleResult{}, errors.New("output path must be absolute")
	}
	if req.Duration <= 0 {
		req.Duration = 4 * time.Second
	}
	switch runtime.GOOS {
	case "darwin":
		return sampleMacOS(ctx, req)
	case "linux":
		return sampleLinuxPerf(ctx, req)
	default:
		return SampleResult{}, fmt.Errorf("direct target sampling is unsupported on %s", runtime.GOOS)
	}
}

// ParseMacOSSample extracts hot functions from `/usr/bin/sample` text output.
// It prefers the "Sort by top of stack" section (self weights per frame) and
// falls back to call-graph frame counts when that section is missing or
// empty. Names are deduplicated keeping their largest observed weight.
func ParseMacOSSample(report string) []Function {
	if functions := parseMacOSSampleTopOfStack(report); len(functions) > 0 {
		return functions
	}
	return parseMacOSSampleCallGraph(report)
}

var macSampleTOSLine = regexp.MustCompile(`^\s*(\d+)\s+(\S+)(?:\s+\(in [^)]*\))?\s*$`)

const macSampleTOSSection = "Sort by top of stack"

func parseMacOSSampleTopOfStack(report string) []Function {
	start := strings.Index(report, macSampleTOSSection)
	if start < 0 {
		return nil
	}
	section := report[start+len(macSampleTOSSection):]
	weights := map[string]int{}
	for _, line := range strings.Split(section, "\n") {
		match := macSampleTOSLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count, err := strconv.Atoi(match[1])
		if err != nil || count <= 0 {
			continue
		}
		name := macSampleSymbol(match[2])
		if name == "" {
			continue
		}
		if count > weights[name] {
			weights[name] = count
		}
	}
	return rankWeights(weights)
}

var macSampleFrameLine = regexp.MustCompile(`^\s*(\d+)\s+([A-Za-z_~][^\s(+]*)(?:\s|$)`)

func parseMacOSSampleCallGraph(report string) []Function {
	weights := map[string]int{}
	for _, line := range strings.Split(report, "\n") {
		match := macSampleFrameLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count, err := strconv.Atoi(match[1])
		if err != nil || count <= 0 {
			continue
		}
		name := macSampleSymbol(match[2])
		if name == "" {
			continue
		}
		if count > weights[name] {
			weights[name] = count
		}
	}
	return rankWeights(weights)
}

// macSampleSymbol strips C-style argument lists from symbols such as
// `free(void*)`, drops process-lifecycle noise, and rejects thread headers.
func macSampleSymbol(raw string) string {
	name := raw
	if idx := strings.Index(name, "("); idx > 0 && !strings.HasPrefix(name, "_Z") {
		name = name[:idx]
	}
	name = strings.TrimPrefix(name, "0x")
	switch name {
	case "", "start", "thread_start", "_start", "main_thread", "_pthread_start":
		return ""
	}
	if strings.Contains(name, "Thread_") || strings.Contains(name, "DispatchQueue") {
		return "" // Mach thread and queue headers, not symbols.
	}
	if _, err := strconv.ParseFloat(name, 64); err == nil {
		return "" // Thread ids and hex addresses are not symbols.
	}
	return name
}

// ParsePerfScript extracts hot functions from `perf script` output. Every
// frame of every event contributes one unit of weight; consecutive duplicate
// frames within a single stack (recursion markers emitted by perf) count once.
func ParsePerfScript(output string) []Function {
	weights := map[string]int{}
	var last string
	inEvent := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inEvent = false
			continue
		}
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			// Event header: `comm pid tid cpu: event:` — starts a new stack.
			inEvent = true
			last = ""
			continue
		}
		if !inEvent {
			continue
		}
		name := perfScriptFrame(trimmed)
		if name == "" {
			continue
		}
		if name == last {
			continue
		}
		last = name
		weights[name]++
	}
	return rankWeights(weights)
}

// perfScriptFrame parses one indented frame like
// `7ff6a1 main (/path/to/binary)` and returns the bare symbol name.
func perfScriptFrame(frame string) string {
	fields := strings.Fields(frame)
	if len(fields) < 2 {
		return ""
	}
	name := fields[1]
	if idx := strings.Index(name, "+0x"); idx > 0 {
		name = name[:idx]
	}
	switch strings.ToLower(name) {
	case "", "[unknown]", "unknown":
		return ""
	}
	return name
}

// rankWeights converts a name->weight map into the Function summary shape,
// ordered hottest first with deterministic tie-breaking by name.
func rankWeights(weights map[string]int) []Function {
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if weights[names[i]] != weights[names[j]] {
			return weights[names[i]] > weights[names[j]]
		}
		return names[i] < names[j]
	})
	functions := make([]Function, 0, len(names))
	for _, name := range names {
		functions = append(functions, Function{Flat: strconv.Itoa(weights[name]), Name: name})
	}
	return functions
}

const maxSamplerOutputBytes = 8 << 20

func sampleMacOS(ctx context.Context, req SampleTarget) (SampleResult, error) {
	sampler := req.SampleBinary
	if sampler == "" {
		sampler = "/usr/bin/sample"
	}
	if _, err := os.Stat(sampler); err != nil {
		return SampleResult{}, fmt.Errorf("macOS sample tool unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, req.Duration+15*time.Second)
	defer cancel()

	workDir, cleanup, err := prepareWorkDir(req)
	if err != nil {
		return SampleResult{}, err
	}
	defer cleanup()

	target := exec.Command(req.BinaryPath, req.Args...)
	target.Dir = workDir
	target.Stdin = bytes.NewReader(req.Stdin)
	target.Stdout = nil
	target.Stderr = nil
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := target.Start(); err != nil {
		return SampleResult{}, fmt.Errorf("start target binary: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := target.Process.Signal(syscall.Signal(0)); err != nil {
		_, _ = target.Process.Wait()
		return SampleResult{}, errors.New("target exited before sampling began")
	}

	temp, err := os.CreateTemp("", "gotorque-sample-*.txt")
	if err != nil {
		terminate(target.Process)
		return SampleResult{}, err
	}
	_ = temp.Close()
	defer os.Remove(temp.Name())

	durationSeconds := strconv.Itoa(int(req.Duration.Seconds() + 1))
	sampleCmd := exec.CommandContext(ctx, sampler, strconv.Itoa(target.Process.Pid), durationSeconds, "-file", temp.Name())
	output, sampleErr := runBounded(sampleCmd)
	waitErr := terminate(target.Process)
	if sampleErr != nil {
		return SampleResult{}, fmt.Errorf("sample pid %d: %v: %s", target.Process.Pid, sampleErr, truncateForError(output))
	}
	if waitErr != nil {
		return SampleResult{}, fmt.Errorf("reap sampled target: %w", waitErr)
	}
	raw, err := os.ReadFile(temp.Name())
	if err != nil {
		return SampleResult{}, fmt.Errorf("read sample report: %w", err)
	}
	return finishSampleResult("macos-sample", req.OutputPath, string(raw))
}

func sampleLinuxPerf(ctx context.Context, req SampleTarget) (SampleResult, error) {
	perf, err := exec.LookPath("perf")
	if err != nil {
		return SampleResult{}, errors.New("perf is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, req.Duration+15*time.Second)
	defer cancel()

	workDir, cleanup, err := prepareWorkDir(req)
	if err != nil {
		return SampleResult{}, err
	}
	defer cleanup()

	dataFile := filepath.Join(filepath.Dir(req.OutputPath), "perf.data.tmp")
	defer os.Remove(dataFile)

	args := append([]string{"record", "-q", "-F", "999", "-e", "cpu-clock",
		"-o", dataFile, "--", req.BinaryPath}, req.Args...)
	record := exec.CommandContext(ctx, perf, args...)
	record.Dir = workDir
	record.Stdin = bytes.NewReader(req.Stdin)
	output, recordErr := runBounded(record)
	if recordErr != nil {
		return SampleResult{}, fmt.Errorf("perf record: %v: %s", recordErr, truncateForError(output))
	}
	script := exec.CommandContext(ctx, perf, "script", "-i", dataFile)
	scriptOutput, scriptErr := runBounded(script)
	if scriptErr != nil {
		return SampleResult{}, fmt.Errorf("perf script: %v: %s", scriptErr, truncateForError(scriptOutput))
	}
	return finishSampleResult("linux-perf", req.OutputPath, string(scriptOutput))
}

// prepareWorkDir materializes seed fixtures into a scratch directory the
// target can read relative paths from, mirroring runner behavior loosely:
// direct sampling stays best-effort, so absolute fixture layouts used by the
// sandbox runner are out of scope here.
func prepareWorkDir(req SampleTarget) (string, func(), error) {
	root := req.TempRoot
	if root == "" {
		root = os.TempDir()
	}
	if !filepath.IsAbs(root) {
		return "", nil, errors.New("temporary root must be absolute")
	}
	dir, err := os.MkdirTemp(root, "gotorque-sample-work-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for rel, content := range req.Fixtures {
		cleanRel := filepath.Clean("/" + rel)
		path := filepath.Join(dir, cleanRel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dir, cleanup, nil
}

func runBounded(cmd *exec.Cmd) ([]byte, error) {
	var buffer limitedWriter
	cmd.Stdout = &buffer
	cmd.Stderr = &buffer
	err := cmd.Run()
	if buffer.exceeded {
		return buffer.b.Bytes(), fmt.Errorf("sampler output exceeded %d bytes", maxSamplerOutputBytes)
	}
	return buffer.b.Bytes(), err
}

type limitedWriter struct {
	b        bytes.Buffer
	exceeded bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > maxSamplerOutputBytes {
		w.exceeded = true
		return 0, errors.New("output limit exceeded")
	}
	return w.b.Write(p)
}

func terminate(process *os.Process) error {
	if process == nil {
		return nil
	}
	_ = process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { _, err := process.Wait(); done <- err }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = process.Kill()
		return <-done
	}
}

func finishSampleResult(sampler, outputPath, raw string) (SampleResult, error) {
	if strings.TrimSpace(raw) == "" {
		return SampleResult{}, errors.New("sampler produced no output")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return SampleResult{}, err
	}
	limit := raw
	if len(limit) > maxSamplerOutputBytes {
		limit = limit[:maxSamplerOutputBytes]
	}
	if err := os.WriteFile(outputPath, []byte(limit), 0o600); err != nil {
		return SampleResult{}, err
	}
	var functions []Function
	if sampler == "macos-sample" {
		functions = ParseMacOSSample(limit)
	} else {
		functions = ParsePerfScript(limit)
	}
	if len(functions) == 0 {
		return SampleResult{}, errors.New("no recognizable frames in sampler output")
	}
	return SampleResult{Sampler: sampler, Functions: functions, RawReport: outputPath}, nil
}

func truncateForError(output []byte) string {
	const limit = 512
	text := strings.TrimSpace(string(output))
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}
