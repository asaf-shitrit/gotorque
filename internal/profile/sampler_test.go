package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func loadRecorded(t *testing.T, name string) Transcript {
	t.Helper()
	transcript, err := LoadTranscript(filepath.Join("testdata", "transcripts", name+".json"))
	require.NoError(t, err)
	return transcript
}

func requireFailure(t *testing.T, err error, kind FailureKind) *SampleError {
	t.Helper()
	var failure *SampleError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, kind, failure.Kind)
	return failure
}

func TestClassifyAcceptsARecordedCPUBoundSample(t *testing.T) {
	result, err := Classify(loadRecorded(t, "macos-busy"))
	require.NoError(t, err)
	require.Equal(t, SamplerMacOS, result.Sampler)
	require.NotEmpty(t, result.Stacks)
	names := make([]string, 0, len(result.Functions))
	for _, fn := range result.Functions {
		names = append(names, fn.Name)
	}
	require.Contains(t, names, "main.hot", "the function the program spends its time in")
}

func TestClassifyCarriesIsolationNotes(t *testing.T) {
	transcript := loadRecorded(t, "macos-busy")
	transcript.IsolationNotes = []string{"memory limit not enforced"}
	result, err := Classify(transcript)
	require.NoError(t, err)
	require.Equal(t, []string{"memory limit not enforced"}, result.IsolationNotes)
}

func TestClassifyLinuxPerfScript(t *testing.T) {
	result, err := Classify(Transcript{Sampler: SamplerLinuxPerf, Report: perfScriptOutput})
	require.NoError(t, err)
	require.Equal(t, SamplerLinuxPerf, result.Sampler)
	require.NotEmpty(t, result.Functions)
}

func TestClassifyRejectsTranscriptsThatAreNotASample(t *testing.T) {
	tests := []struct {
		name       string
		transcript Transcript
		kind       FailureKind
		sentinel   error
		detail     string
	}{
		{"tool missing", Transcript{Sampler: SamplerLinuxPerf, Unavailable: "perf is not installed"}, FailUnavailable, nil, "perf is not installed"},
		{"target gone before attach", Transcript{Sampler: SamplerMacOS, ExitedBeforeAttach: true}, FailExitedEarly, ErrTargetExitedEarly, "target exited before sampling began"},
		{"sampler failed", Transcript{Sampler: SamplerLinuxPerf, Label: "perf record", ExitStatus: 1, Output: "boom"}, FailFailed, nil, "perf record: exit status 1: boom"},
		{"no report", Transcript{Sampler: SamplerMacOS, Report: "  \n"}, FailFailed, nil, "sampler produced no output"},
		{"empty call graph", Transcript{Sampler: SamplerMacOS, Report: "Call graph:\n"}, FailNoFrames, ErrNoFrames, "no recognizable frames"},
		{"perf output with no frames", Transcript{Sampler: SamplerLinuxPerf, Report: "no frames here\n"}, FailNoFrames, ErrNoFrames, "no recognizable frames"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Classify(tt.transcript)
			failure := requireFailure(t, err, tt.kind)
			require.Contains(t, failure.Error(), tt.detail)
			if tt.sentinel != nil {
				require.ErrorIs(t, err, tt.sentinel)
			}
			for _, other := range []error{ErrNoFrames, ErrTargetExitedEarly} {
				if !errors.Is(other, tt.sentinel) {
					require.NotErrorIs(t, err, other)
				}
			}
		})
	}
}

func TestSampleKeepsTheRawReportAndRetriesAnEmptyCallGraphOnce(t *testing.T) {
	busy := loadRecorded(t, "macos-busy")
	sampler := NewReplay(Transcript{Sampler: SamplerMacOS, Report: "Call graph:\n"}, busy)
	out := filepath.Join(t.TempDir(), "nested", "report.txt")
	result, err := Sample(context.Background(), sampler, SampleTarget{BinaryPath: "/bin/true", OutputPath: out})
	require.NoError(t, err)
	require.Len(t, sampler.Requests(), 2)
	require.Equal(t, out, result.RawReport)
	saved, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, busy.Report, string(saved), "the retry's report replaces the empty one")
}

func TestSampleRetriesAnEmptyCallGraphOnlyOnce(t *testing.T) {
	sampler := NewReplay(Transcript{Sampler: SamplerMacOS, Report: "Call graph:\n"})
	_, err := Sample(context.Background(), sampler, SampleTarget{BinaryPath: "/bin/true", OutputPath: filepath.Join(t.TempDir(), "r.txt")})
	require.ErrorIs(t, err, ErrNoFrames)
	require.Len(t, sampler.Requests(), 2)
}

func TestSampleDoesNotRetryOtherFailures(t *testing.T) {
	sampler := NewReplay(Transcript{Sampler: SamplerMacOS, ExitedBeforeAttach: true})
	_, err := Sample(context.Background(), sampler, SampleTarget{BinaryPath: "/bin/true", OutputPath: filepath.Join(t.TempDir(), "r.txt")})
	require.ErrorIs(t, err, ErrTargetExitedEarly)
	require.Len(t, sampler.Requests(), 1)
}

func TestSamplePassesAnAdapterErrorThrough(t *testing.T) {
	boom := errors.New("scratch directory")
	sampler := SamplerFunc(func(context.Context, SampleTarget) (Transcript, error) { return Transcript{}, boom })
	_, err := Sample(context.Background(), sampler, SampleTarget{BinaryPath: "/bin/true", OutputPath: filepath.Join(t.TempDir(), "r.txt")})
	require.ErrorIs(t, err, boom)
}

func TestSampleValidatesTheRequestBeforeSampling(t *testing.T) {
	sampler := NewReplay()
	for name, req := range map[string]SampleTarget{
		"no binary":       {OutputPath: "/tmp/r.txt"},
		"relative binary": {BinaryPath: "bin", OutputPath: "/tmp/r.txt"},
		"no output":       {BinaryPath: "/bin/true"},
		"relative output": {BinaryPath: "/bin/true", OutputPath: "r.txt"},
	} {
		_, err := Sample(context.Background(), sampler, req)
		require.Error(t, err, name)
	}
	require.Empty(t, sampler.Requests())
}

func TestLoadTranscriptReportsMissingFiles(t *testing.T) {
	_, err := LoadTranscript(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "t.json"), []byte(`{"report_file":"absent.txt"}`), 0o600))
	_, err = LoadTranscript(filepath.Join(dir, "t.json"))
	require.Error(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{`), 0o600))
	_, err = LoadTranscript(filepath.Join(dir, "bad.json"))
	require.Error(t, err)
}

func TestReplayWithNoTranscriptsFailsTheCall(t *testing.T) {
	_, err := NewReplay().Sample(context.Background(), SampleTarget{})
	require.Error(t, err)
}

func TestPlatformSamplerReportsAnUnsupportedHostUnavailable(t *testing.T) {
	transcript, err := unsupportedSampler{os: "plan9"}.Sample(context.Background(), SampleTarget{})
	require.NoError(t, err)
	_, err = Classify(transcript)
	_ = requireFailure(t, err, FailUnavailable)
	require.ErrorContains(t, err, "plan9")
}
