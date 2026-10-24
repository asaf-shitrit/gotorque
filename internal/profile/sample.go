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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/runner"
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

	// Sandbox is the campaign's sandbox policy (the target manifest's
	// sandbox block). Direct sampling cannot apply network or filesystem
	// isolation -- it needs a live PID to attach a sampler to, which the
	// sandboxed runner's exec wrapping is not built for -- but it does apply
	// the same environment allow/passthrough filtering and resource limits,
	// so a sampled run does not see variables or headroom a measured run
	// would not have. The zero value grants only the sampler's own minimal
	// HOME/TMPDIR base.
	Sandbox runner.SandboxPolicy
}

type SampleResult struct {
	Sampler   string
	Functions []Function
	// Stacks are the sampled call paths, leaf first, for attributing samples
	// to the function that made the calls (AttributeToOwn).
	Stacks    []Stack
	RawReport string
	// IsolationNotes records anything the sandbox policy's environment or
	// resource limits could not be fully enforced for this sampled run, in
	// the same style as runner.RunResult.IsolationNotes.
	IsolationNotes []string
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

// Each entry is `symbol  (in binary)  count`, with the count right-aligned at
// the end of the line. Reading the count from the front matched nothing a real
// report ever prints: the shipped fixture was hand-written in a shape
// /usr/bin/sample does not emit, so every macOS campaign parsed zero hot
// functions, fell back to zero excerpts, and optimized blind.
//
// The symbol is everything before ` (in <binary>)`, not the first run of
// non-space characters. A generic method instantiated over an interface shape
// prints its whole type argument, spaces included
// (`persist.(*TransientSlice[go.shape.interface { Eval(…) … }]).Set`). Taking
// the first space merged Set, Append and Resize into one truncated name that
// resolved to no declaration, so ivy's hottest frames were never offered.
var macSampleTOSLine = regexp.MustCompile(`^\s*(.+?)\s+\(in [^()]*\)\s+(\d+)\s*$`)

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
		count, err := strconv.Atoi(match[2])
		if err != nil || count <= 0 {
			continue
		}
		name := macSampleSymbol(match[1])
		if name == "" {
			continue
		}
		if count > weights[name] {
			weights[name] = count
		}
	}
	return rankWeights(weights)
}

// Call-graph rows are indented one level per frame and prefixed with `+` for
// the sampled frame and `!` for a truncated branch, so the count never starts
// the line. The old pattern required it to, which left only thread headers
// matching and made this fallback useless on a real report. Rows are read by
// parseCallGraphRow so this fallback accepts the same frame text the stacks
// parser does, generic instantiations included.
func parseMacOSSampleCallGraph(report string) []Function {
	weights := map[string]int{}
	for _, line := range strings.Split(report, "\n") {
		_, node, ok := parseCallGraphRow(line)
		if !ok {
			continue
		}
		count := node.count
		name := node.frame
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
//
// A parenthesis that follows a dot belongs to a Go receiver
// (`github.com/itchyny/gojq.(*env).Next`), not to an argument list, so cutting
// there reduced every Go method frame to its package prefix and left the hot
// list naming functions that do not exist.
// A parenthesis inside a generic instantiation's type arguments belongs to the
// instantiation, not to an argument list, so only a parenthesis at bracket
// depth zero ends the symbol.
func macSampleSymbol(raw string) string {
	name := raw
	if idx := macArgListStart(name); idx > 0 {
		name = name[:idx]
	}
	name = normalizeShapeArgs(name)
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

// macArgListStart returns where a C-style argument list opens the symbol, or
// -1 when the symbol carries none. Brackets and braces are tracked because a
// generic method's symbol spells out the whole shape it was instantiated over
// (`[go.shape.interface { Eval(…) …, String() string }]`), and those
// parentheses name the shape's methods rather than an argument list.
func macArgListStart(name string) int {
	if strings.HasPrefix(name, "_Z") {
		return -1 // An Itanium C++ mangled name keeps its parentheses.
	}
	depth := 0
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '(':
			if depth > 0 || (i > 0 && name[i-1] == '.') {
				continue
			}
			return i
		}
	}
	return -1
}

// normalizeShapeArgs rewrites a generic instantiation over an interface shape
// to pprof's `[...]`, so every instantiation of one generic function reports
// as one symbol and resolves to one declaration.
func normalizeShapeArgs(name string) string {
	const shape = "[go.shape."
	if !strings.Contains(name, shape) {
		return name
	}
	var b strings.Builder
	for {
		idx := strings.Index(name, shape)
		if idx < 0 {
			b.WriteString(name)
			return b.String()
		}
		end := closingBracket(name, idx)
		if end < 0 {
			b.WriteString(name)
			return b.String()
		}
		b.WriteString(name[:idx])
		b.WriteString("[...]")
		name = name[end+1:]
	}
}

// closingBracket returns the index of the `]` closing the `[` at start, or -1.
// A shape argument embeds a whole interface or struct type, whose method set
// and field list may hold brackets of their own.
func closingBracket(s string, start int) int {
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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
// `7ff6a1 main (/path/to/binary)` and returns the bare symbol name. The symbol
// runs from after the address to the offset and module columns that follow it,
// so a generic instantiation keeps the spaces in its type argument; taking the
// second whitespace-separated field cut it at the first of them, the same
// truncation the macOS parser had.
func perfScriptFrame(frame string) string {
	_, rest, ok := strings.Cut(strings.TrimLeft(frame, " \t"), " ")
	if !ok {
		return ""
	}
	name := strings.TrimSpace(rest)
	for _, marker := range []string{" (+0x", " (/"} {
		if idx := strings.Index(name, marker); idx > 0 {
			name = name[:idx]
		}
	}
	if idx := strings.Index(name, "+0x"); idx > 0 {
		name = name[:idx]
	}
	switch strings.ToLower(name) {
	case "", "[unknown]", "unknown":
		return ""
	}
	return normalizeShapeArgs(name)
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

// sampleMacOS is the macOS adapter's process plumbing: it starts the target,
// attaches /usr/bin/sample, and returns what happened. Judging the transcript
// is Classify's job. These helpers all carry the sampleMacOS prefix because the
// Makefile excludes that name from the CRAP gate: only macOS can run them.
func sampleMacOS(ctx context.Context, req SampleTarget, sampler string) (Transcript, error) {
	transcript := Transcript{Sampler: SamplerMacOS}
	if sampler == "" {
		sampler = "/usr/bin/sample"
	}
	if _, err := os.Stat(sampler); err != nil {
		transcript.Unavailable = fmt.Sprintf("macOS sample tool unavailable: %v", err)
		return transcript, nil
	}
	ctx, cancel := context.WithTimeout(ctx, req.Duration+15*time.Second)
	defer cancel()

	workDir, cleanup, err := prepareWorkDir(req)
	if err != nil {
		return Transcript{}, err
	}
	defer cleanup()

	target, notes, err := sampleMacOSStart(ctx, req, workDir)
	if err != nil {
		return Transcript{}, err
	}
	transcript.IsolationNotes = notes
	if target == nil {
		transcript.ExitedBeforeAttach = true
		return transcript, nil
	}
	if err := sampleMacOSAttach(ctx, sampler, req, target, &transcript); err != nil {
		return Transcript{}, err
	}
	return transcript, nil
}

// sampleMacOSStart starts the target and gives it half a second, the
// sampler's minimum. It returns a nil process when the target is already gone.
func sampleMacOSStart(ctx context.Context, req SampleTarget, workDir string) (*os.Process, []string, error) {
	// Started with exec.Command rather than exec.CommandContext deliberately:
	// this target outlives the sampler call and is reaped by terminate() through
	// os.Process.Wait, never Cmd.Wait. CommandContext starts a watcher goroutine
	// that only finishes once Wait drains its unbuffered result channel, so every
	// sample would leak that goroutine and retain the Cmd until the process
	// exits. Cancellation still reaches the target: the sampler runs under ctx,
	// and every path out of sampleMacOS reaps the target.
	//
	binaryPath, args, limitNotes := runner.WrapWithResourceLimits(ctx, req.Sandbox.Limits, req.BinaryPath, req.Args)
	//nolint:noctx // reaped by terminate(), not Cmd.Wait; CommandContext would leak a watcher goroutine
	target := exec.Command(binaryPath, args...)
	target.Dir = workDir
	target.Stdin = bytes.NewReader(req.Stdin)
	target.Stdout = nil
	target.Stderr = nil
	target.Env = sampleEnv(req, workDir)
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := target.Start(); err != nil {
		return nil, nil, fmt.Errorf("start target binary: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if target.Process.Signal(syscall.Signal(0)) == nil {
		return target.Process, limitNotes, nil
	}
	_, _ = target.Process.Wait()
	return nil, limitNotes, nil
}

// sampleMacOSAttach runs /usr/bin/sample against the live target, reaps the
// target, and records the sampler's report or its exit status on the
// transcript.
func sampleMacOSAttach(ctx context.Context, sampler string, req SampleTarget, target *os.Process, transcript *Transcript) error {
	temp, err := os.CreateTemp("", "gotorque-sample-*.txt")
	if err != nil {
		_ = terminate(target)
		return err
	}
	_ = temp.Close()
	defer func() { _ = os.Remove(temp.Name()) }()

	durationSeconds := strconv.Itoa(int(req.Duration.Seconds() + 1))
	sampleCmd := exec.CommandContext(ctx, sampler, strconv.Itoa(target.Pid), durationSeconds, "-file", temp.Name())
	output, sampleErr := runBounded(sampleCmd)
	waitErr := terminate(target)
	transcript.Label = fmt.Sprintf("sample pid %d", target.Pid)
	transcript.Output = string(output)
	if status, ok := exitStatus(sampleErr); ok {
		transcript.ExitStatus = status
		return nil
	}
	if sampleErr != nil {
		return fmt.Errorf("%s: %w: %s", transcript.Label, sampleErr, truncateForError(output))
	}
	if waitErr != nil {
		return fmt.Errorf("reap sampled target: %w", waitErr)
	}
	raw, err := os.ReadFile(temp.Name())
	if err != nil {
		return fmt.Errorf("read sample report: %w", err)
	}
	transcript.Report = string(raw)
	return nil
}

// exitStatus extracts a process's own non-zero exit status from the error its
// Run returned. A signal, a failed start or an output overflow carries none:
// those are plumbing failures, not something the sampler said.
func exitStatus(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// sampleEnv builds the target's environment from the sandbox policy's
// allow/passthrough variables plus a minimal HOME/TMPDIR base rooted at the
// sample's own scratch directory, the same shape runner.Sandbox.Env uses.
// Leaving Cmd.Env unset would inherit gotorque's own process environment,
// including a model provider credential such as OPENROUTER_API_KEY; this
// keeps a sampled run's environment no broader than a measured run's.
func sampleEnv(req SampleTarget, workDir string) []string {
	base := []string{"HOME=" + workDir, "TMPDIR=" + workDir, "TMP=" + workDir, "TEMP=" + workDir}
	return runner.BuildEnv(req.Sandbox.Environment, base, nil)
}

// sampleLinuxPerf is the Linux adapter's process plumbing: `perf record`
// around the target, then `perf script` over the data. Excluded from the CRAP
// gate (Makefile) because only Linux can run it.
func sampleLinuxPerf(ctx context.Context, req SampleTarget) (Transcript, error) {
	transcript := Transcript{Sampler: SamplerLinuxPerf}
	perf, err := exec.LookPath("perf")
	if err != nil {
		transcript.Unavailable = "perf is not installed"
		return transcript, nil
	}
	ctx, cancel := context.WithTimeout(ctx, req.Duration+15*time.Second)
	defer cancel()

	workDir, cleanup, err := prepareWorkDir(req)
	if err != nil {
		return Transcript{}, err
	}
	defer cleanup()

	dataFile := filepath.Join(filepath.Dir(req.OutputPath), "perf.data.tmp")
	defer func() { _ = os.Remove(dataFile) }()

	// perf record is never wrapped in the memory rlimit shell: it profiles
	// the target directly, and the sandbox's max_memory_bytes is left
	// unenforced for the duration of a sampled run -- recorded below --
	// rather than bounding perf's own address space alongside the target's.
	transcript.IsolationNotes = runner.ProfilingResourceLimitNotes(req.Sandbox.Limits)
	recordArgs := append([]string{"record", "-q", "-F", "999", "-e", "cpu-clock",
		"-o", dataFile, "--", req.BinaryPath}, req.Args...)
	record := exec.CommandContext(ctx, perf, recordArgs...)
	record.Dir = workDir
	record.Stdin = bytes.NewReader(req.Stdin)
	record.Env = sampleEnv(req, workDir)
	output, recordErr := runBounded(record)
	if failed, err := perfStep(&transcript, "perf record", output, recordErr); failed || err != nil {
		return transcript, err
	}
	script := exec.CommandContext(ctx, perf, "script", "-i", dataFile)
	scriptOutput, scriptErr := runBounded(script)
	if failed, err := perfStep(&transcript, "perf script", scriptOutput, scriptErr); failed || err != nil {
		return transcript, err
	}
	transcript.Report = string(scriptOutput)
	return transcript, nil
}

// perfStep records a failed perf step on the transcript. It reports whether
// the step failed with an exit status the transcript now carries, or returns
// the plumbing error when it failed without one.
func perfStep(transcript *Transcript, label string, output []byte, err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if status, ok := exitStatus(err); ok {
		transcript.Label, transcript.ExitStatus, transcript.Output = label, status, string(output)
		return true, nil
	}
	return false, fmt.Errorf("%s: %w: %s", label, err, truncateForError(output))
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

func truncateForError(output []byte) string {
	const limit = 512
	text := strings.TrimSpace(string(output))
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}
