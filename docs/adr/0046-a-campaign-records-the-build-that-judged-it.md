# 0046. A campaign records the gotorque build that judged it

- Status: accepted
- Date: 2026-10-25

## Context

A campaign directory names the target's revision (`State.Environment.Revision`)
and nothing about gotorque itself. `internal/version.Commit` is `unknown`
unless the release build stamps it through ldflags, and no campaign state held
the value: `go build ./cmd/gotorque` followed by an `optimize` run produced a
directory that could not say which engine reached its verdicts.

Verdicts depend on that engine. Twelve engine pull requests merged into `main`
between 2026-10-24 and 2026-10-25, and several change a verdict on the same
evidence: #112 and #123 replicate a guardrail breach before it rejects, #115
re-measures an accept that rested on the best of several readings, #110 reports
an outlier-robust shift instead of the mean, #121 changes which seeds
acceptance may rest on, #128 decides a peak-memory breach on the fresh series.
`gotorque scorecard` groups campaign directories by whatever it is handed, so
campaigns from before and after those merges can land in one false-accept rate
with nothing on screen to say the group is mixed. The same holds for one
campaign: `--resume` and `gotorque verify` can each run on a different binary
than the campaign they continue, and a verification's decision is a verdict.

Issue #125 asked for the record and left three questions open. Triage answered
them by the issue's own constraint (the build record "must not change a
verdict"): record and report, never refuse.

## Decision

**Persist the build that started a campaign, and the build of every run after
it. Show them; never act on them.**

- `internal/version.Build` is `{Version, Commit, Modified}` and
  `CurrentBuild()` fills it: a commit the release build set through ldflags
  (`.goreleaser.yaml`) wins, otherwise the Go VCS stamp
  (`debug.ReadBuildInfo`, `vcs.revision` and `vcs.modified`) supplies it. The
  modified flag is carried in every case, because a verdict measured on a
  checkout with uncommitted changes is not a claim about any commit.
- `State.Build` holds the build that created the campaign;
  `State.LaterBuilds` holds one `BuildRun{At, Build}` per resume and verify
  run. Both entry points go through `campaign.Resume`, which appends and saves
  before it composes the engine, so a run is recorded even when it fails
  straight away.
- The fields are additive, and `checkSchemaVersion` still reads
  `ReportSchemaVersion` 1: a campaign written before this change loads,
  resumes, verifies and scores, and reports its build as `unknown`. Bumping the
  schema would have refused exactly the campaigns the record exists to
  explain.
- `report` prints `- Build:` next to the target's `- Revision:`, and a
  `## Builds` table when later runs exist.
- `scorecard` adds a `build` column to both tables and prints
  `> Builds differ: ...` above them when the grouped campaigns ran on more than
  one build. It never refuses and changes no count: grouping by build stays the
  caller's job, the same way held-out versus development splitting already
  does.

Nothing here is read by `internal/policy`, by the evaluator or by any bound. A
verdict is exactly what it was before this change.

## Evidence

- `TestResumeRecordsTheBuildItRunsOn` writes the state an old campaign leaves
  behind (no `Build`, no `LaterBuilds`), resumes it, and asserts the loaded
  record names the current build in one `LaterBuilds` entry; `TestResumeAppends
  EachRunRatherThanReplacingTheLast` resumes twice and asserts two entries.
- `TestCampaignRunsPersistsReportsAndResumes` asserts a fresh campaign's
  `State.Build` equals `version.CurrentBuild()`.
- `TestBuildFrom*` cover the four sources: release ldflags, the VCS stamp
  `go build` leaves, a modified checkout, and a binary with neither (which
  `Describe` renders as `unknown`).
- `TestReportShowsTheBuildBesideTheTargetRevision`,
  `TestReportMarksADirtyBuild`, `TestReportListsEveryBuildThatResumedTheCampaign`
  and `TestReportShowsABuildFromBeforeRecordingAsUnknown` pin the report.
- `TestScorecardNamesTheBuildEachCampaignRanOn`,
  `TestScorecardWarnsWhenCampaignsRanOnDifferentBuilds`,
  `TestScorecardDoesNotWarnOnOneBuild` and
  `TestScorecardGroupsCampaignsFromBeforeRecordingTogether` pin the scorecard,
  including that the warning leaves the counts alone.

Unverified: no campaign was run to produce a real mixed-build scorecard. The
release path is covered by `buildFrom`'s unit tests, not by a goreleaser build;
the VCS fallback is what the repository's own test binaries exercise.

## Consequences

- A campaign directory answers "which engine judged this?" with no extra bookkeeping
  and no migration. Where the answer is genuinely unknown it says `unknown`
  rather than guessing a build from timestamps.
- A scorecard over a mixed set now says so, so the stability numbers and the
  engine that produced them move together. The record feeds #103, which asks
  the README to name the revision a held-out sweep measured.
- `verify` gains no new flag: it records its build like any resume, and the
  report's `## Builds` table shows the campaign's set of binaries.
- Campaign state grows by one small object per run. A campaign resumed often
  grows by that much and no more.

## Alternatives considered

- **Refuse to group campaigns from different builds, or refuse `--resume` on a
  different build** (the issue's two open questions). Refusing would make the
  record a policy input, which the issue forbids: an operator on a newer build
  could not resume or verify a campaign at all, and the old data would become
  unreadable exactly when it is most needed.
- **Identify a build by its release tag only.** Most campaigns run from a
  checkout, where there is no tag. The VCS stamp is the only identifier a plain
  `go build` carries.
- **Record a timestamp instead and infer the build.** Timestamps do not say
  which binary ran; several builds can exist at one moment.
- **Bump `ReportSchemaVersion`.** It would refuse the campaigns already on
  disk, and an added field a reader can treat as absent is not a shape change.
- **Record the build in `report.json` only.** The report is a snapshot rewritten
  per verdict; state is what resume, verify and scorecard load. Anything that
  must survive a resume belongs in bbolt (CLAUDE.md).
