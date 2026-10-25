package campaign

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/version"
)

// mustTime parses a timestamp a test writes into state.
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

// recordedBuild is a campaign started by one build, so a report or a scorecard
// test can name something other than the process's own build.
var recordedBuild = version.Build{Version: "dev", Commit: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed"}

func TestReportShowsTheBuildBesideTheTargetRevision(t *testing.T) {
	state := State{ID: "c", SchemaVersion: ReportSchemaVersion, Repository: "/repo", Build: recordedBuild,
		Environment: Environment{Revision: "1234567", Authority: "measured", OS: "darwin", Architecture: "arm64"}}
	out := RenderMarkdown(state)
	// The two revisions are different things: Build is gotorque's own, Revision
	// is the target's commit. A reader who confuses them cannot check which
	// build reached the verdict.
	require.Contains(t, out, "- Build: `dev 9b1fea7`\n- Revision: `1234567`\n")
	require.NotContains(t, out, "## Builds", "one build ran the whole campaign, so the table says nothing extra")
}

func TestReportShowsABuildFromBeforeRecordingAsUnknown(t *testing.T) {
	state := State{ID: "c", SchemaVersion: ReportSchemaVersion, Repository: "/repo",
		Environment: Environment{Revision: "1234567", Authority: "measured", OS: "darwin", Architecture: "arm64"}}
	out := RenderMarkdown(state)
	require.Contains(t, out, "- Build: `unknown`\n- Revision: `1234567`\n")
}

func TestReportMarksADirtyBuild(t *testing.T) {
	build := recordedBuild
	build.Modified = true
	state := State{ID: "c", SchemaVersion: ReportSchemaVersion, Build: build,
		Environment: Environment{Authority: "measured", OS: "darwin", Architecture: "arm64"}}
	require.Contains(t, RenderMarkdown(state), "- Build: `dev 9b1fea7-dirty`")
}

func TestReportListsEveryBuildThatResumedTheCampaign(t *testing.T) {
	later := version.Build{Version: "0.2.0", Commit: "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90"}
	state := State{ID: "c", SchemaVersion: ReportSchemaVersion, Build: recordedBuild,
		Environment: Environment{Authority: "measured", OS: "darwin", Architecture: "arm64"},
		StartedAt:   mustTime(t, "2026-10-25T09:00:00Z"),
		LaterBuilds: []BuildRun{{At: mustTime(t, "2026-10-25T11:30:00Z"), Build: later}},
	}
	out := RenderMarkdown(state)
	require.Contains(t, out, "## Builds")
	require.Contains(t, out, "| campaign | 2026-10-25T09:00:00Z | `dev 9b1fea7` |")
	require.Contains(t, out, "| run 2 | 2026-10-25T11:30:00Z | `0.2.0 d72af7f` |")
}

// buildRecordedCampaign is a campaign whose later runs a test can name.
func loadCampaignState(t *testing.T, dir string) State {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	state, err := store.Load()
	require.NoError(t, err)
	require.NoError(t, store.Close())
	return state
}

func TestResumeRecordsTheBuildOnceTheRunStartsWork(t *testing.T) {
	// writePastCampaign writes the state a campaign from before build
	// recording leaves behind: no Build, no LaterBuilds.
	dir := writePastCampaign(t, "past", "rev", []CandidateRecord{{Attempt: 1, Decision: domain.DecisionInconclusive}})
	resumed, err := Resume(dir, io.Discard)
	require.NoError(t, err)
	require.Empty(t, resumed.State().LaterBuilds, "opening the directory measures nothing, so it records no build")
	require.NoError(t, resumed.recordBuild())
	state := resumed.State()
	require.Equal(t, version.Unknown, state.Build.Describe(), "an old campaign keeps its honest gap")
	require.Len(t, state.LaterBuilds, 1)
	require.Equal(t, version.CurrentBuild(), state.LaterBuilds[0].Build)
	require.False(t, state.LaterBuilds[0].At.IsZero())

	// Persisted as the run starts, so a run that dies at once still records
	// which build touched it. The engine holds the database lock until it is
	// closed, so the record is read back afterwards.
	require.NoError(t, resumed.Close())
	loaded := loadCampaignState(t, dir)
	require.Len(t, loaded.LaterBuilds, 1)
	require.Equal(t, version.CurrentBuild(), loaded.LaterBuilds[0].Build)
}

func TestResumeRecordsOneBuildForAProcessWithSeveralAttempts(t *testing.T) {
	dir := writePastCampaign(t, "past", "rev", nil)
	resumed, err := Resume(dir, io.Discard)
	require.NoError(t, err)
	// A verify command runs one attempt per accepted candidate, and all of
	// them are the same process: one run, one build entry.
	require.NoError(t, resumed.recordBuild())
	require.NoError(t, resumed.recordBuild())
	require.Len(t, resumed.State().LaterBuilds, 1)
	require.NoError(t, resumed.Close())
	require.Len(t, loadCampaignState(t, dir).LaterBuilds, 1)
}

func TestResumeAppendsEachRunRatherThanReplacingTheLast(t *testing.T) {
	dir := writePastCampaign(t, "past", "rev", nil)
	for range 2 {
		resumed, err := Resume(dir, io.Discard)
		require.NoError(t, err)
		require.NoError(t, resumed.recordBuild())
		require.NoError(t, resumed.Close())
	}
	require.Len(t, loadCampaignState(t, dir).LaterBuilds, 2)
}

func TestScorecardNamesTheBuildEachCampaignRanOn(t *testing.T) {
	one := State{ID: "one", Build: recordedBuild}
	one.Manifest.Name = "tool CLI"
	later := version.Build{Version: "0.2.0", Commit: "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90"}
	two := State{ID: "two", Build: recordedBuild, LaterBuilds: []BuildRun{{At: mustTime(t, "2026-10-25T11:30:00Z"), Build: later}}}
	two.Manifest.Name = "tool CLI"

	rows := []ScorecardRow{scorecardRow("one", one), scorecardRow("two", two)}
	require.Equal(t, []string{"dev 9b1fea7"}, rows[0].Builds)
	require.Equal(t, []string{"dev 9b1fea7", "0.2.0 d72af7f"}, rows[1].Builds, "a campaign continued on another build names both")

	out := RenderScorecard(rows)
	require.Contains(t, out, "| one | tool CLI | dev 9b1fea7 |")
	require.Contains(t, out, "| two | tool CLI | dev 9b1fea7, 0.2.0 d72af7f |")
}

func TestScorecardWarnsWhenCampaignsRanOnDifferentBuilds(t *testing.T) {
	other := version.Build{Version: "0.2.0", Commit: "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90"}
	mine := State{ID: "mine", Build: recordedBuild, CandidateRecords: []CandidateRecord{{Attempt: 1, Accepted: true, Decision: domain.DecisionAccepted}}}
	mine.Manifest.Name = "tool CLI"
	theirs := State{ID: "theirs", Build: other}
	theirs.Manifest.Name = "tool CLI"

	out := RenderScorecard([]ScorecardRow{scorecardRow("mine", mine), scorecardRow("theirs", theirs)})
	require.Contains(t, out, "> Builds differ: these campaigns ran on 2 gotorque builds (`dev 9b1fea7`, `0.2.0 d72af7f`)")
	require.Contains(t, out, "Group them by build before comparing.")
	// The warning is a note, not a gate: the verdicts are still counted.
	require.Contains(t, out, "| **total** | 2 campaigns |  | 1 | 0 | 0 | 0 | 0 | 0 | 0 | 0/0 |")
}

func TestScorecardDoesNotWarnOnOneBuild(t *testing.T) {
	state := State{ID: "one", Build: recordedBuild}
	out := RenderScorecard([]ScorecardRow{scorecardRow("one", state)})
	require.NotContains(t, out, "Builds differ")
	require.NotContains(t, out, "dirty checkout")
}

func TestScorecardWarnsWhenAGroupedBuildIsDirty(t *testing.T) {
	// Two dirty builds of one commit read the same name, so the differing
	// builds warning cannot fire; the rate still mixes two engines.
	dirty := recordedBuild
	dirty.Modified = true
	one := State{ID: "one", Build: dirty}
	two := State{ID: "two", Build: dirty}
	rows := []ScorecardRow{scorecardRow("one", one), scorecardRow("two", two)}

	require.Equal(t, []string{"dev 9b1fea7-dirty"}, rows[0].Builds)
	require.True(t, rows[0].DirtyBuild)
	out := RenderScorecard(rows)
	require.NotContains(t, out, "Builds differ")
	require.Contains(t, out, "> Builds include a dirty checkout: a binary built from uncommitted changes names the revision it sat on, not the code it ran.")
	require.Contains(t, out, "Two dirty builds of one revision read the same here")
}

func TestScorecardFindsADirtyBuildFromALaterRun(t *testing.T) {
	dirty := recordedBuild
	dirty.Modified = true
	state := State{ID: "one", Build: recordedBuild, LaterBuilds: []BuildRun{{At: mustTime(t, "2026-10-25T11:30:00Z"), Build: dirty}}}
	require.True(t, scorecardRow("one", state).DirtyBuild, "a resume from a dirty checkout marks the campaign too")
}

func TestScorecardGroupsCampaignsFromBeforeRecordingTogether(t *testing.T) {
	first := State{ID: "first"}
	second := State{ID: "second"}
	out := RenderScorecard([]ScorecardRow{scorecardRow("first", first), scorecardRow("second", second)})
	require.NotContains(t, out, "Builds differ", "two unrecorded campaigns ran on two builds we cannot name; claiming they are one build would be worse")
	require.Equal(t, []string{version.Unknown}, scorecardRow("first", first).Builds)
	require.Equal(t, 2, strings.Count(out, "| unknown |"))
}

func TestBuildCellWithoutTheField(t *testing.T) {
	require.Equal(t, version.Unknown, buildCell(ScorecardRow{}))
	require.Equal(t, "dev 9b1fea7", buildCell(ScorecardRow{Builds: []string{"dev 9b1fea7"}}))
}
