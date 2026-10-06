package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// FailureKind classifies why a transcript is not a usable sample.
type FailureKind string

const (
	// FailExitedEarly: the target ended before, or while, the sampler attached.
	FailExitedEarly FailureKind = "exited_early"
	// FailNoFrames: the sampler attached and recorded no frame at all.
	FailNoFrames FailureKind = "no_frames"
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
		return SampleResult{}, &SampleError{Kind: FailFailed, Detail: fmt.Sprintf("%s: exit status %d: %s", labelOr(t.Label, "sampler"), t.ExitStatus, truncateForError([]byte(t.Output)))}
	}
	if strings.TrimSpace(t.Report) == "" {
		return SampleResult{}, &SampleError{Kind: FailFailed, Detail: "sampler produced no output"}
	}
	report := boundReport(t.Report)
	functions, stacks := parseReport(t.Sampler, report)
	if len(functions) == 0 {
		return SampleResult{}, &SampleError{Kind: FailNoFrames, Detail: ErrNoFrames.Error()}
	}
	return SampleResult{Sampler: t.Sampler, Functions: functions, Stacks: stacks, IsolationNotes: t.IsolationNotes}, nil
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

// PlatformSampler returns the adapter for the host operating system.
func PlatformSampler() (Sampler, error) {
	switch runtime.GOOS {
	case "darwin":
		return MacOSSampler{}, nil
	case "linux":
		return LinuxPerfSampler{}, nil
	}
	return nil, fmt.Errorf("direct target sampling is unsupported on %s", runtime.GOOS)
}

// SampleTargetProfile samples the target with the host's own sampler.
func SampleTargetProfile(ctx context.Context, req SampleTarget) (SampleResult, error) {
	sampler, err := PlatformSampler()
	if err != nil {
		return SampleResult{}, err
	}
	return Sample(ctx, sampler, req)
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
	if errors.Is(err, ErrNoFrames) {
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
