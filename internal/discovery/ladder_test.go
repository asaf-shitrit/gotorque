package discovery

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// recordedBusy loads the transcript of a real sample of a CPU-bound program.
func recordedBusy(t *testing.T) profile.Transcript {
	t.Helper()
	transcript, err := profile.LoadTranscript(filepath.Join("..", "profile", "testdata", "transcripts", "macos-busy.json"))
	require.NoError(t, err)
	return transcript
}

var gone = profile.Transcript{Sampler: profile.SamplerMacOS, ExitedBeforeAttach: true}

func newLadder(t *testing.T, sampler profile.Sampler, seeds ...manifest.SeedWorkload) *Ladder {
	t.Helper()
	return NewLadder(Inputs{BinaryPath: "/bin/target", Command: []string{"run"}, Seeds: seeds, Dir: t.TempDir()}, sampler)
}

// TestFirstLivingFallsBackToALongerStressSeed: a seed whose input is files runs
// only as long as its files make it, and the macOS sampler cannot attach to a
// process that exits at once. Discovery then samples the manifest's stress
// seed instead of giving up. The sampler is scripted, so this runs on any OS:
// the quick seeds find the target already gone, the long one is sampled.
func TestFirstLivingFallsBackToALongerStressSeed(t *testing.T) {
	busy := recordedBusy(t)
	sampler := profile.SamplerFunc(func(_ context.Context, req profile.SampleTarget) (profile.Transcript, error) {
		if slices.Contains(req.Args, "long") {
			return busy, nil
		}
		return gone, nil
	})
	seeds := []manifest.SeedWorkload{
		{ID: "quick", Tier: domain.TierRepresentative, Args: []string{"quick"}},
		{ID: "medium", Tier: domain.TierPlausible, Args: []string{"medium"}},
		{ID: "long", Tier: domain.TierStress, Args: []string{"long"}},
	}
	ladder := newLadder(t, sampler, seeds...)
	seed, result, err := ladder.FirstLiving(context.Background())
	require.NoError(t, err)
	require.Equal(t, "long", seed.ID)
	require.NotEmpty(t, result.Functions)

	ladder = newLadder(t, sampler, seeds[:2]...)
	_, _, err = ladder.FirstLiving(context.Background())
	require.ErrorContains(t, err, "quick: ", "with no stress seed the first seed's failure is reported")
	require.NotContains(t, err.Error(), "medium", "only stress seeds are tried after the first")
}

func TestFirstLivingSamplesTheCommandThenTheSeedsArguments(t *testing.T) {
	replay := profile.NewReplay(recordedBusy(t))
	ladder := newLadder(t, replay, manifest.SeedWorkload{ID: "only", Args: []string{"-v", "in.txt"}})
	_, _, err := ladder.FirstLiving(context.Background())
	require.NoError(t, err)
	requests := replay.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, "/bin/target", requests[0].BinaryPath)
	require.Equal(t, []string{"run", "-v", "in.txt"}, requests[0].Args)
	require.Equal(t, filepath.Join(ladder.in.Dir, "profile-sample", "sample-report.txt"), requests[0].OutputPath)
}

// TestARepeatableSeedThatEndsTooSoonIsSampledOnALargerInput: held-out csvq kept
// busy for 0.48s on 16 MiB, just short of the sampler's half second, so the
// same seed gets one more try at eight times the size.
func TestARepeatableSeedThatEndsTooSoonIsSampledOnALargerInput(t *testing.T) {
	replay := profile.NewReplay(gone, recordedBusy(t))
	seed := manifest.SeedWorkload{ID: "csv", Files: []manifest.FixtureFile{{Path: "rows.csv", Header: "a,b\n", Content: "1,2\n", Repeat: 10}}}
	_, result, err := newLadder(t, replay, seed).FirstLiving(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, result.Functions)
	requests := replay.Requests()
	require.Len(t, requests, 2)
	first, second := len(requests[0].Fixtures["rows.csv"]), len(requests[1].Fixtures["rows.csv"])
	require.Greater(t, first, AmplificationTarget/2)
	require.InDelta(t, 8, float64(second)/float64(first), 0.1, "the retry is eight times the first input")
}

func TestAScriptSeedIsNotRetriedLarger(t *testing.T) {
	replay := profile.NewReplay(gone)
	seed := manifest.SeedWorkload{ID: "script", Files: []manifest.FixtureFile{{Path: "a.lua", Content: "print(1)\n"}}}
	_, _, err := newLadder(t, replay, seed).FirstLiving(context.Background())
	require.ErrorContains(t, err, profile.ErrTargetExitedEarly.Error())
	require.Len(t, replay.Requests(), 1, "a script cannot grow")
}

// TestVariantFallsBackToItsInputOneCopyPerLine: gron --stream reads one
// document per line, so the 16 MiB single-line document ends it before the
// sampler attaches; the line-repeated input is what it can chew on.
func TestVariantFallsBackToItsInputOneCopyPerLine(t *testing.T) {
	replay := profile.NewReplay(gone, recordedBusy(t))
	ladder := newLadder(t, replay)
	variant := manifest.SeedWorkload{ID: "seed --stream", Stdin: `{"users":[1,2]}`}
	results := ladder.Variants(context.Background(), []manifest.SeedWorkload{variant})
	require.Len(t, results, 1)
	requests := replay.Requests()
	require.Len(t, requests, 2)
	require.Equal(t, filepath.Join(ladder.in.Dir, "profile-sample", "sample-report-1.txt"), requests[1].OutputPath)
	require.Equal(t, RepeatLines([]byte(variant.Stdin)), requests[1].Stdin)
	require.GreaterOrEqual(t, bytes.Count(requests[1].Stdin, []byte("\n")), 2)
	events := ladder.TakeEvents()
	require.Len(t, events, 1)
	require.Equal(t, "workload_sample_input", events[0].Kind)
	require.Contains(t, events[0].Message, "seed --stream sampled on its input repeated one copy per line")
	require.Empty(t, ladder.TakeEvents(), "events are handed over once")
}

func TestAVariantThatCannotBeSampledIsRecordedAndSkipped(t *testing.T) {
	replay := profile.NewReplay(gone, gone, recordedBusy(t))
	ladder := newLadder(t, replay)
	variants := []manifest.SeedWorkload{
		{ID: "bad", Stdin: "x"},
		{ID: "good", Stdin: "y"},
	}
	results := ladder.Variants(context.Background(), variants)
	require.Len(t, results, 1, "the good variant is sampled after the bad one is left out")
	events := ladder.TakeEvents()
	require.Len(t, events, 1)
	require.Equal(t, "workload_sample_skipped", events[0].Kind)
	require.Contains(t, events[0].Message, "bad could not be sampled")
	require.Contains(t, events[0].Message, "with one copy per line")
}

func TestAVariantWithNoStdinHasNoLineRepetitionToTry(t *testing.T) {
	replay := profile.NewReplay(gone)
	ladder := newLadder(t, replay)
	require.Empty(t, ladder.Variants(context.Background(), []manifest.SeedWorkload{{ID: "files-only"}}))
	require.Len(t, replay.Requests(), 1)
}

func TestLadderCollectsIsolationNotesFromSampledRuns(t *testing.T) {
	busy := recordedBusy(t)
	busy.IsolationNotes = []string{"max_memory_bytes not enforced while sampling"}
	ladder := newLadder(t, profile.NewReplay(busy), manifest.SeedWorkload{ID: "only"})
	_, _, err := ladder.FirstLiving(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"max_memory_bytes not enforced while sampling"}, ladder.Notes())
}
