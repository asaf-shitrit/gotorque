package discovery

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// recorded loads a sampler transcript recorded from a real run.
func recorded(t *testing.T, name string) profile.Transcript {
	t.Helper()
	transcript, err := profile.LoadTranscript(filepath.Join("..", "profile", "testdata", "transcripts", name+".json"))
	require.NoError(t, err)
	return transcript
}

// TestASamplerThatLostTheTargetIsRetriedOnALargerInput is held-out s2c and
// csvq, and gron and yq: /usr/bin/sample exited 255 "cannot examine process"
// on a target that ended while it attached, nothing was retried, and discovery
// profiled the library's benchmarks instead of the CLI's own path.
func TestASamplerThatLostTheTargetIsRetriedOnALargerInput(t *testing.T) {
	replay := profile.NewReplay(recorded(t, "macos-cannot-examine"), recordedBusy(t))
	seed := manifest.SeedWorkload{ID: "compress-logs", Files: []manifest.FixtureFile{{Path: "logs.txt", Content: "line\n", Repeat: 10}}}
	_, result, err := newLadder(t, replay, seed).FirstLiving(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, result.Functions)
	requests := replay.Requests()
	require.Len(t, requests, 2)
	require.Greater(t, len(requests[1].Fixtures["logs.txt"]), 4*len(requests[0].Fixtures["logs.txt"]), "the retry is on a larger input")
}
