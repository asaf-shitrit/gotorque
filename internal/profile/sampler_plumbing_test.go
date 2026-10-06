package profile

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExitStatusSeparatesASamplersVerdictFromPlumbingFailures(t *testing.T) {
	status, ok := exitStatus(exec.CommandContext(context.Background(), "sh", "-c", "exit 3").Run())
	require.True(t, ok)
	require.Equal(t, 3, status)
	_, ok = exitStatus(nil)
	require.False(t, ok)
	_, ok = exitStatus(errors.New("start failed"))
	require.False(t, ok)
}

func TestPerfStepRecordsAnExitStatusAndReturnsPlumbingErrors(t *testing.T) {
	var transcript Transcript
	failed, err := perfStep(&transcript, "perf record", nil, nil)
	require.NoError(t, err)
	require.False(t, failed)

	exit := exec.CommandContext(context.Background(), "sh", "-c", "exit 2").Run()
	failed, err = perfStep(&transcript, "perf script", []byte("no samples"), exit)
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, Transcript{Label: "perf script", ExitStatus: 2, Output: "no samples"}, transcript)

	failed, err = perfStep(&Transcript{}, "perf record", []byte("out"), errors.New("fork failed"))
	require.ErrorContains(t, err, "perf record: fork failed: out")
	require.False(t, failed)
}
