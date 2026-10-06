package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Sampler is the seam between discovery and the operating system's sampling
// tool. An adapter only runs processes: it starts the target, attaches the
// tool, and hands back what happened as a Transcript. It decides nothing about
// whether that run was a usable profile; Classify does, in code any platform
// can test, so the judgment never hides inside an adapter that only one
// operating system can execute.
//
// An adapter returns an error only for a failure it cannot describe in a
// transcript (a scratch directory it could not create, a target it could not
// start, a canceled context).
type Sampler interface {
	Sample(ctx context.Context, req SampleTarget) (Transcript, error)
}

// Names an adapter stamps on its Transcript, selecting the report format
// Classify parses.
const (
	SamplerMacOS     = "macos-sample"
	SamplerLinuxPerf = "linux-perf"
)

// Transcript is everything a sampler run produced, recorded without judgment.
// The scripted adapter replays transcripts recorded from real sampler runs.
type Transcript struct {
	// Sampler is the adapter's name (SamplerMacOS, SamplerLinuxPerf).
	Sampler string `json:"sampler"`
	// Report is the sampler's report text: `sample`'s call graph, or `perf
	// script`'s event listing. Empty when the sampler never produced one.
	Report string `json:"report,omitempty"`
	// Label names the sampler process ExitStatus and Output describe, such
	// as "sample pid 4242" or "perf record", so a failure can say which one.
	Label string `json:"label,omitempty"`
	// ExitStatus and Output are the sampler process's exit status and its
	// combined stdout and stderr.
	ExitStatus int    `json:"exit_status,omitempty"`
	Output     string `json:"output,omitempty"`
	// ExitedBeforeAttach is set when the target was already gone when the
	// sampler was about to attach to it. It is the negation of "alive at
	// attach" so that the zero value is the ordinary case.
	ExitedBeforeAttach bool `json:"exited_before_attach,omitempty"`
	// Unavailable says why the sampling tool could not be used at all, such
	// as a missing binary.
	Unavailable string `json:"unavailable,omitempty"`
	// IsolationNotes records anything the sandbox policy could not enforce
	// for this run (see SampleResult.IsolationNotes).
	IsolationNotes []string `json:"isolation_notes,omitempty"`
}

// ErrNoFrames marks sampler output with no recognizable frame: a sampler that
// attached and recorded nothing, transiently or because the target finished
// as sampling began.
var ErrNoFrames = errors.New("no recognizable frames in sampler output")

// ErrTargetExitedEarly reports a target that finished before the sampler
// could attach: on macOS it has to be alive half a second after it starts.
// The caller can give it a larger input and try again.
var ErrTargetExitedEarly = errors.New("target exited before sampling began")

// ErrIdle marks a sample that caught the target waiting: frames exist, but
// none is the program's own code or a dependency's, only runtime and kernel
// wait frames. It is a property of when the sampler looked (the target was
// still reading its input), not of the target, so discovery treats it as it
// treats an empty call graph.
var ErrIdle = errors.New("sample caught the target idle")

// FailureKind classifies why a transcript is not a usable sample.
type FailureKind string

const (
	// FailExitedEarly: the target ended before, or while, the sampler attached.
	FailExitedEarly FailureKind = "exited_early"
	// FailNoFrames: the sampler attached and recorded no frame at all.
	FailNoFrames FailureKind = "no_frames"
	// FailIdle: frames were recorded, but only idle runtime and wait frames.
	FailIdle FailureKind = "idle"
	// FailUnavailable: the sampling tool cannot run on this host.
	FailUnavailable FailureKind = "unavailable"
	// FailFailed: the sampler failed in a way no other kind explains.
	FailFailed FailureKind = "failed"
)

// SampleError is the typed reason Classify rejects a transcript. errors.Is
// matches it against ErrTargetExitedEarly and ErrNoFrames, the sentinels
// discovery's retries key off.
type SampleError struct {
	Kind   FailureKind
	Detail string
}

func (f *SampleError) Error() string { return f.Detail }

func (f *SampleError) Is(target error) bool {
	switch f.Kind {
	case FailExitedEarly:
		return target == ErrTargetExitedEarly
	case FailNoFrames:
		return target == ErrNoFrames
	case FailIdle:
		return target == ErrIdle
	case FailUnavailable, FailFailed:
		return false
	}
	return false
}

// Classify maps a transcript to a sample or to the typed SampleError that says why
// it is not one. It is pure: no process, file or clock.
func Classify(t Transcript) (SampleResult, error) {
	if t.Unavailable != "" {
		return SampleResult{}, &SampleError{Kind: FailUnavailable, Detail: t.Unavailable}
	}
	if t.ExitedBeforeAttach {
		return SampleResult{}, &SampleError{Kind: FailExitedEarly, Detail: ErrTargetExitedEarly.Error()}
	}
	if t.ExitStatus != 0 {
		return SampleResult{}, samplerExit(t)
	}
	if strings.TrimSpace(t.Report) == "" {
		return SampleResult{}, &SampleError{Kind: FailFailed, Detail: "sampler produced no output"}
	}
	report := boundReport(t.Report)
	functions, stacks := parseReport(t.Sampler, report)
	if len(functions) == 0 {
		return SampleResult{}, &SampleError{Kind: FailNoFrames, Detail: ErrNoFrames.Error()}
	}
	if !hasProgramFrame(stacks, functions) {
		return SampleResult{}, &SampleError{Kind: FailIdle, Detail: ErrIdle.Error() + ": only runtime and wait frames, none from the program or its dependencies"}
	}
	return SampleResult{Sampler: t.Sampler, Functions: functions, Stacks: stacks, IsolationNotes: t.IsolationNotes}, nil
}

// lostTarget are the ways /usr/bin/sample says the target ended while it was
// attaching: it exits 255 with "cannot examine process" and one of these. The
// same message also reports a refused permission, which is a real failure and
// is not on this list.
var lostTarget = []string{
	"no longer appears to be running",
	"for unknown reasons, even though it appears to exist",
}

// samplerExit classifies a sampler process that exited non-zero: the target
// ending under it is the race a larger input answers, anything else a failure.
func samplerExit(t Transcript) *SampleError {
	detail := fmt.Sprintf("%s: exit status %d: %s", labelOr(t.Label, "sampler"), t.ExitStatus, truncateForError([]byte(t.Output)))
	if slices.ContainsFunc(lostTarget, func(reason string) bool { return strings.Contains(t.Output, reason) }) {
		return &SampleError{Kind: FailExitedEarly, Detail: ErrTargetExitedEarly.Error() + ": " + detail}
	}
	return &SampleError{Kind: FailFailed, Detail: detail}
}

func labelOr(label, fallback string) string {
	if label == "" {
		return fallback
	}
	return label
}

func parseReport(sampler, report string) ([]Function, []Stack) {
	if sampler == SamplerMacOS {
		return ParseMacOSSample(report), MacOSSampleStacks(report)
	}
	return ParsePerfScript(report), PerfScriptStacks(report)
}

func boundReport(report string) string {
	if len(report) > maxSamplerOutputBytes {
		return report[:maxSamplerOutputBytes]
	}
	return report
}

// MacOSSampler samples with /usr/bin/sample.
type MacOSSampler struct {
	// Binary overrides /usr/bin/sample (tests only).
	Binary string
}

func (s MacOSSampler) Sample(ctx context.Context, req SampleTarget) (Transcript, error) {
	return sampleMacOS(ctx, req, s.Binary)
}

// LinuxPerfSampler samples with `perf record` and `perf script`.
type LinuxPerfSampler struct{}

func (LinuxPerfSampler) Sample(ctx context.Context, req SampleTarget) (Transcript, error) {
	return sampleLinuxPerf(ctx, req)
}

// PlatformSampler returns the adapter for the host operating system. On a host
// with no adapter its transcripts report the sampler unavailable, so discovery
// falls back to benchmarks rather than failing.
func PlatformSampler() Sampler {
	switch runtime.GOOS {
	case "darwin":
		return MacOSSampler{}
	case "linux":
		return LinuxPerfSampler{}
	}
	return unsupportedSampler{os: runtime.GOOS}
}

type unsupportedSampler struct{ os string }

func (s unsupportedSampler) Sample(context.Context, SampleTarget) (Transcript, error) {
	return Transcript{Unavailable: "direct target sampling is unsupported on " + s.os}, nil
}

// SampleTargetProfile samples the target with the host's own sampler.
func SampleTargetProfile(ctx context.Context, req SampleTarget) (SampleResult, error) {
	return Sample(ctx, PlatformSampler(), req)
}

// Sample runs the sampler against the target and classifies the transcript,
// preserving the raw report at req.OutputPath.
func Sample(ctx context.Context, sampler Sampler, req SampleTarget) (SampleResult, error) {
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
	result, err := sampleOnce(ctx, sampler, req)
	if errors.Is(err, ErrNoFrames) || errors.Is(err, ErrIdle) {
		// An idle sample is the same kind of miss: pup's first campaign
		// sampled only parked threads while the target still read its input.
		//
		// A sampler that attached to a live target and still recorded no
		// frame failed transiently: on a dasel campaign /usr/bin/sample
		// wrote an empty call graph for a target that ran 13 s, the
		// campaign had no hot function and no benchmark to fall back on,
		// and every candidate was written blind. The same binary and input
		// sampled 102 functions when run again.
		result, err = sampleOnce(ctx, sampler, req)
	}
	return result, err
}

func sampleOnce(ctx context.Context, sampler Sampler, req SampleTarget) (SampleResult, error) {
	transcript, err := sampler.Sample(ctx, req)
	if err != nil {
		return SampleResult{}, err
	}
	if err := saveReport(req.OutputPath, transcript.Report); err != nil {
		return SampleResult{}, err
	}
	result, err := Classify(transcript)
	if err != nil {
		return SampleResult{}, err
	}
	result.RawReport = req.OutputPath
	return result, nil
}

// saveReport preserves the sampler's report, so a sample that classified as a
// failure can still be read.
func saveReport(path, report string) error {
	if strings.TrimSpace(report) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(boundReport(report)), 0o600)
}

// hasProgramFrame reports whether any sampled frame is code of the program
// under test: the stacks when the report yielded them, else the frames it
// listed.
func hasProgramFrame(stacks []Stack, functions []Function) bool {
	for _, stack := range stacks {
		if slices.ContainsFunc(stack.Frames, programFrame) {
			return true
		}
	}
	return slices.ContainsFunc(functions, func(fn Function) bool { return programFrame(fn.Name) })
}

// programFrame reports whether a symbol is the program's own code or a
// dependency's: package main, or a package whose import path starts at a host
// (github.com/..., golang.org/x/...). A standard library package has no dot in
// its first path element, as do the runtime's own frames and the kernel's
// (__psynch_cvwait, kevent). A sample of nothing but those is the program
// waiting, however many of its samples a stray sync.(*Pool).Get accounts for.
// A module whose path has no dot is read as standard library, so such a module
// that never reaches package main would sample as idle; every campaign target
// has a hosted path, and its command is package main.
func programFrame(symbol string) bool {
	pkg := SymbolPackage(symbol)
	if pkg == "main" {
		return true
	}
	host, _, _ := strings.Cut(pkg, "/")
	return strings.Contains(host, ".")
}
