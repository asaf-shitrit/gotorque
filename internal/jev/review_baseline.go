package jev

// reviewBaseline is Jev's typical answer to each hazard question over 93 real,
// merged single-function performance patches (the benchmark's golang/go and
// GitHub fixes), measured 2026-09-27 against the pinned build (jev.Model)
// through OpenRouter's System One API (ADR 0030), with no labels. Valid only
// for the exact questions and review state it was measured with;
// TestReviewBaselineMatchesQuestions fails when either changes. Regenerate
// with TestLiveReviewBaseline.
var reviewBaseline = map[Hazard]stats{
	HazardOutputOrder:    {mean: 0.0457, std: 0.0295},
	HazardErrorBehavior:  {mean: 0.1513, std: 0.1670},
	HazardDroppedError:   {mean: 0.0885, std: 0.1753},
	HazardSkippedEffect:  {mean: 0.1512, std: 0.1668},
	HazardBufferAliasing: {mean: 0.0643, std: 0.0567},
	HazardConcurrency:    {mean: 0.0590, std: 0.0330},
	HazardNumericOutput:  {mean: 0.0532, std: 0.0448},
	HazardOffTarget:      {mean: 0.1425, std: 0.1153},
}

const reviewBaselineDigest = "55dfa5aa3b5e9541cee1389f433191c9eb52dcac79ff972daee52088ed11fa8d"

// This baseline is measured against the same pinned build (jev.Model) as the
// cause baseline in baseline.go; there is only one Jev version to pin, not
// one per role.
