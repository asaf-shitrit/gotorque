package jev

// baseline is Jev's typical answer to each question: the mean and standard
// deviation of its probabilities over 186 real Go functions, the before and
// after versions of the 93 single-function performance fixes in the benchmark
// (golang/go history plus GitHub unbuffered-IO fixes), measured 2026-09-27
// against the pinned build (jev.Model) through OpenRouter's System One API
// (ADR 0030). It uses no labels, only the spread of Jev's own answers, so it
// is safe to reuse for any target. The means are why raw answers cannot be
// compared: Jev's average yes runs from 0.10 for unbuffered IO to 0.52 for
// allocation.
//
// It is valid only for the exact question text and state template it was
// measured with; TestBaselineMatchesQuestions fails when either changes.
// Regenerate with TestLiveBaseline (see baseline_live_test.go) and paste its
// output here.
var baseline = map[Cause]stats{
	CauseAlloc:        {mean: 0.5187, std: 0.1991},
	CauseUnbufferedIO: {mean: 0.1023, std: 0.1693},
	CauseStringBuild:  {mean: 0.1780, std: 0.1836},
	CauseFastPath:     {mean: 0.1609, std: 0.1362},
	CauseSuperlinear:  {mean: 0.1689, std: 0.1496},
	CausePrealloc:     {mean: 0.2288, std: 0.2021},
	CauseRedundant:    {mean: 0.2953, std: 0.1730},
}

const baselineDigest = "56b2f3c6240ba65b0822b6e5fe8088f6b971aeaf788780c63fa8fcb8f771c841"
