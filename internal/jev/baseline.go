package jev

// baseline is Jev's typical answer to each question: the mean and standard
// deviation of its probabilities over 186 real Go functions, the before and
// after versions of the 93 single-function performance fixes in the benchmark
// (golang/go history plus GitHub unbuffered-IO fixes), measured 2026-09-22. It
// uses no labels, only the spread of Jev's own answers, so it is safe to reuse
// for any target. The means are why raw answers cannot be compared: Jev's
// average yes runs from 0.10 for unbuffered IO to 0.52 for allocation.
//
// It is valid only for the exact question text and state template it was
// measured with; TestBaselineMatchesQuestions fails when either changes.
// Regenerate with TestLiveBaseline (see baseline_live_test.go) and paste its
// output here.
var baseline = map[Cause]stats{
	CauseAlloc:        {mean: 0.5211, std: 0.1989},
	CauseUnbufferedIO: {mean: 0.1037, std: 0.1718},
	CauseStringBuild:  {mean: 0.1783, std: 0.1838},
	CauseFastPath:     {mean: 0.1610, std: 0.1359},
	CauseSuperlinear:  {mean: 0.1708, std: 0.1527},
	CausePrealloc:     {mean: 0.2269, std: 0.2009},
	CauseRedundant:    {mean: 0.2965, std: 0.1742},
}

const baselineDigest = "8030163fc4fcbfcf50c4589ba8154d83b094573e0e4a9c9ea9716058fdbc5932"

// baselineModelRelease is the release_date GET <gateway base>/typesafe/v1/models
// reported for the "jev" entry when this baseline (and reviewBaseline in
// review_baseline.go, measured the same day) were recorded. It is the only
// version signal the gateway exposes (see ADR 0012, ADR 0029): Client.Preflight
// compares it on every run under --analyst/--reviewer/--explorer jev and fails
// on a mismatch (EnvAllowDrift downgrades that to a warning), because nothing
// else here would notice a Jev release shipped behind the unversioned
// "typesafe-ai/jev" alias.
const baselineModelRelease = "2026-09-15"
