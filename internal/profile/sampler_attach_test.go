package profile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClassifyReadsASamplerThatLostTheTargetAsExitedEarly: /usr/bin/sample
// exits 255 with "cannot examine process" when the target ends while it is
// attaching, which is the race ErrTargetExitedEarly already names. Left
// unclassified it was a plain failure, so discovery never retried on a larger
// input and fell back to benchmarks (issue 81: s2c, csvq, gron, yq).
func TestClassifyReadsASamplerThatLostTheTargetAsExitedEarly(t *testing.T) {
	for _, name := range []string{"macos-exited-process", "macos-cannot-examine"} {
		t.Run(name, func(t *testing.T) {
			transcript := loadRecorded(t, name)
			require.Equal(t, 255, transcript.ExitStatus)
			_, err := Classify(transcript)
			failure := requireFailure(t, err, FailExitedEarly)
			require.ErrorIs(t, err, ErrTargetExitedEarly)
			require.NotErrorIs(t, err, ErrNoFrames)
			require.Contains(t, failure.Error(), "sample cannot examine process", "the sampler's own words stay in the event")
		})
	}
}

// TestClassifyKeepsARealPermissionFailureFailed: not every "cannot examine
// process" is a target that ended. A sampler refused permission to attach is
// no reason to run the target again on a larger input.
func TestClassifyKeepsARealPermissionFailureFailed(t *testing.T) {
	_, err := Classify(Transcript{
		Sampler: SamplerMacOS, Label: "sample pid 1", ExitStatus: 255,
		Output: "sample[85307]: sample cannot examine process 1 (launchd) because you do not have appropriate privileges to examine it; try running with `sudo`.\n",
	})
	_ = requireFailure(t, err, FailFailed)
	require.NotErrorIs(t, err, ErrTargetExitedEarly)
}
