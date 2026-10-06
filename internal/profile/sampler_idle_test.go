package profile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClassifyRejectsASampleThatCaughtTheTargetIdle is the first pup campaign:
// 35 samples, every one a thread parked in a runtime wait, no frame of pup or
// of golang.org/x/net/html. It counted as a successful profile with zero hot
// functions, so the analysis flagged nothing and the campaign finished before
// a single candidate (issue 79). Neither the retry nor the benchmark fallback
// saw it, because both key off an error and this was none.
func TestClassifyRejectsASampleThatCaughtTheTargetIdle(t *testing.T) {
	for _, name := range []string{"macos-pup-idle", "macos-sleeping"} {
		t.Run(name, func(t *testing.T) {
			_, err := Classify(loadRecorded(t, name))
			failure := requireFailure(t, err, FailIdle)
			require.ErrorIs(t, err, ErrIdle)
			require.NotErrorIs(t, err, ErrNoFrames, "it has frames; a different rung of the ladder applies")
			require.Contains(t, failure.Error(), "caught the target idle")
		})
	}
}

// TestClassifyKeepsSamplesWithTheProgramsOwnFrames: waiting is not idleness
// when the program is on the stack. gron's output loop sits mostly in a
// write that the sampler cannot unwind past, and still has main.gron above it.
func TestClassifyKeepsSamplesWithTheProgramsOwnFrames(t *testing.T) {
	_, err := Classify(loadRecorded(t, "macos-busy"))
	require.NoError(t, err)
	for _, file := range []string{"macos-sample-gron-thread.txt", "macos-sample-gojq.txt"} {
		report, err := os.ReadFile(filepath.Join("testdata", file))
		require.NoError(t, err)
		_, err = Classify(Transcript{Sampler: SamplerMacOS, Report: string(report)})
		require.NoError(t, err, file)
	}
}

func TestProgramFrame(t *testing.T) {
	for frame, want := range map[string]string{
		"main.main":                                "main frame",
		"main.(*T).run":                            "main method",
		"github.com/ericchiang/pup.Run":            "module",
		"golang.org/x/net/html.(*Tokenizer).Next":  "dependency",
		"runtime.gopark":                           "",
		"runtime.asmcgocall.abi0":                  "",
		"sync.(*Pool).Get":                         "",
		"os.(*File).Read":                          "",
		"internal/poll.(*FD).Read":                 "",
		"__psynch_cvwait":                          "",
		"write":                                    "",
		"net/http.(*Server).Serve":                 "",
		"github.com/x/y.Generic[go.shape.int].Set": "generic over a dotted path",
	} {
		require.Equal(t, want != "", programFrame(frame), "%s (%s)", frame, want)
	}
}
