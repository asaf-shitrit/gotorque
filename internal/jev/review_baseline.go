package jev

// reviewBaseline is Jev's typical answer to each hazard question over 93 real,
// merged single-function performance patches (the benchmark's golang/go and
// GitHub fixes), measured 2026-09-22 with no labels. Valid only for the exact questions
// and review state it was measured with; TestReviewBaselineMatchesQuestions
// fails when either changes. Regenerate with TestLiveReviewBaseline.
var reviewBaseline = map[Hazard]stats{
	HazardOutputOrder:    {mean: 0.0449, std: 0.0289},
	HazardErrorBehavior:  {mean: 0.1529, std: 0.1650},
	HazardDroppedError:   {mean: 0.0872, std: 0.1735},
	HazardSkippedEffect:  {mean: 0.1492, std: 0.1618},
	HazardBufferAliasing: {mean: 0.0651, std: 0.0590},
	HazardConcurrency:    {mean: 0.0586, std: 0.0328},
	HazardNumericOutput:  {mean: 0.0531, std: 0.0438},
	HazardOffTarget:      {mean: 0.1411, std: 0.1151},
}

const reviewBaselineDigest = "653e2414ab09a20bcbec035aa289ffd818d4fb652a6e053ff4894b44150b757b"

// This baseline was measured the same day, against the same Jev release, as
// the cause baseline in baseline.go: see baselineModelRelease there for the
// release_date Client.Preflight checks against on every --reviewer jev run.
// It is not duplicated here because there is only one Jev version to check,
// not one per role.
