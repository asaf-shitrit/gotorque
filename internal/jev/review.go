package jev

import (
	"fmt"
	"math"
	"sort"
)

// Hazard names one way a performance patch can change a program's behaviour.
// The set mirrors the reviewer prompt's behaviour-hazard checklist.
type Hazard string

const (
	HazardOutputOrder    Hazard = "output_order"
	HazardErrorBehavior  Hazard = "error_behavior"
	HazardDroppedError   Hazard = "dropped_error"
	HazardSkippedEffect  Hazard = "skipped_effect"
	HazardBufferAliasing Hazard = "buffer_aliasing"
	HazardConcurrency    Hazard = "concurrency"
	HazardNumericOutput  Hazard = "numeric_output"
	HazardOffTarget      Hazard = "off_target"
)

// Hazards is the stable order used for questions, ties, and reports.
var Hazards = []Hazard{HazardOutputOrder, HazardErrorBehavior, HazardDroppedError, HazardSkippedEffect, HazardBufferAliasing, HazardConcurrency, HazardNumericOutput, HazardOffTarget}

const reviewContext = "A proposed performance patch to one Go function. The program's observable behaviour must not change."

type hazardSpec struct {
	question Question
	// concern names the hazard in the review's concerns.
	concern string
	// check is the check a reader should run before trusting the patch.
	check string
}

// One yes/no question per hazard, each naming the state field it judges.
// Questions ids never reach the model, so every instruction is complete.
var hazardSpecs = map[Hazard]hazardSpec{
	HazardOutputOrder: {
		question: boolean(
			"Could the change in `patch` make the order of the program's output depend on map iteration order or goroutine scheduling?",
			"After the change, the same input can produce its output items in a different order from run to run.",
			"Output order is still fixed by the input or by an explicit sort, or the change does not affect output order."),
		concern: "output order may depend on map iteration or goroutine scheduling",
		check:   "run the same input repeatedly and diff the outputs byte for byte",
	},
	HazardErrorBehavior: {
		question: boolean(
			"Does the change in `patch` change which error is returned, the text or wrapping of an error message, or an exit status?",
			"Some failing input now yields a different error value, message, or exit code than the code in `source`.",
			"Every path returns the same errors, messages, and exit codes as the code in `source`."),
		concern: "error values, messages, or exit status may change",
		check:   "drive every error path (bad input, failing writer) and compare stderr and exit codes",
	},
	HazardDroppedError: {
		question: boolean(
			"Does the change in `patch` add or keep a call that can fail, such as Flush, Close, or Write, while discarding its error with `_ =` or `defer`?",
			"A failure of that call would be silently lost, so a write error the caller should see goes unreported.",
			"Every error from a call the change introduces is returned or handled, or the change adds no fallible call."),
		concern: "an error from a call that can fail is discarded",
		check:   "write to a writer that fails (a closed pipe, a full disk) and confirm the failure is still reported",
	},
	HazardSkippedEffect: {
		question: boolean(
			"Could the change in `patch` skip, on some path, an effect the code in `source` always performed, such as writing output, flushing, or closing?",
			"On an early return, error path, or goto, work the old code always did no longer happens.",
			"Every path still performs every effect the code in `source` performed."),
		concern: "an effect the old code always performed may be skipped on some path",
		check:   "exercise the early-return and error paths and compare the output written before the failure",
	},
	HazardBufferAliasing: {
		question: boolean(
			"Could the change in `patch` return or keep bytes from a buffer that is later reused or overwritten, such as a sync.Pool buffer or a shared scratch slice?",
			"A returned string or slice shares memory with a buffer another call will overwrite.",
			"Returned values own their memory, or no buffer is reused."),
		concern: "returned bytes may alias a buffer that is reused later",
		check:   "call the function twice and confirm the first result is unchanged after the second call",
	},
	HazardConcurrency: {
		question: boolean(
			"Does the change in `patch` introduce goroutines or shared mutable state that concurrent calls could race on or that could reorder output?",
			"New goroutines, package-level mutable state, or unsynchronized sharing appear in the change.",
			"The change adds no concurrency and no shared mutable state."),
		concern: "new goroutines or shared mutable state could race or reorder output",
		check:   "run the tests with -race and repeat output comparisons under load",
	},
	HazardNumericOutput: {
		question: boolean(
			"Does the change in `patch` change how numbers are parsed, rounded, formatted, or truncated?",
			"Some number can now be printed or stored differently than by the code in `source`.",
			"Numbers are parsed and formatted exactly as before, or the change does not touch numbers."),
		concern: "numbers may be parsed, rounded, or formatted differently",
		check:   "compare output for large, fractional, negative, and exponent-form numbers",
	},
	HazardOffTarget: {
		question: boolean(
			"Does `patch` change more than `hypothesis` needs, such as renaming variables, refactoring unrelated code, or touching other functions?",
			"Parts of the diff are unrelated to the optimization `hypothesis` describes.",
			"Every line of the diff serves the optimization `hypothesis` describes."),
		concern: "the diff changes more than the hypothesis needs",
		check:   "review the lines unrelated to the hypothesis and drop them",
	},
}

// ReviewQuestions returns the hazard questions asked about every patch.
func ReviewQuestions() map[string]Question {
	questions := make(map[string]Question, len(Hazards))
	for _, h := range Hazards {
		questions[string(h)] = hazardSpecs[h].question
	}
	return questions
}

// ReviewState is what the review judges: the optimizer's hypothesis, the
// patched function's source before the patch, and the diff. Nothing else, so
// the answers turn on the change rather than on the program around it.
func ReviewState(hypothesis, function, source, patch string) map[string]string {
	return map[string]string{"context": reviewContext, "hypothesis": hypothesis, "function": function, "source": source, "patch": patch}
}

func (h Hazard) Concern() string { return hazardSpecs[h].concern }

// Check is what a reader should run before trusting a patch with this hazard.
func (h Hazard) Check() string { return hazardSpecs[h].check }

// HazardScore is one hazard's answer for one patch.
type HazardScore struct {
	Hazard      Hazard  `json:"hazard"`
	Probability float64 `json:"probability"`
	Z           float64 `json:"z"`
}

// RankHazards orders a patch's hazards by distance from Jev's usual answer on
// real, merged performance patches. Ties keep the Hazards order.
func RankHazards(answers map[string]Answer) ([]HazardScore, error) {
	scores := make([]HazardScore, 0, len(Hazards))
	for _, h := range Hazards {
		answer, ok := answers[string(h)]
		if !ok {
			return nil, fmt.Errorf("no answer to the %s question", h)
		}
		p := answer.Probability
		if math.IsNaN(p) || p < 0 || p > 1 {
			return nil, fmt.Errorf("answer to the %s question has probability %v outside [0, 1]", h, p)
		}
		b := reviewBaseline[h]
		scores = append(scores, HazardScore{Hazard: h, Probability: p, Z: (p - b.mean) / b.std})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Z > scores[j].Z })
	return scores, nil
}

// HazardThreshold and HazardFloor decide which hazards a review raises: an
// answer far above Jev's usual one for that question, and one it leans yes on.
// On the reviewer benchmark, z alone raised a concern on 31% of 93 real merged
// performance patches; adding the floor cut that to 14%, most of them genuine
// (bufio patches that discard the flush error), while still catching every
// injected hazard whose label was right. With the measured baseline every
// question's usual answer plus two deviations sits below one half, so today the
// floor decides and z orders the concerns; the threshold stays so a future
// baseline with a higher usual answer cannot turn an ordinary yes into noise.
const (
	HazardThreshold = 2.0
	HazardFloor     = 0.5
)

// FlaggedHazards keeps the hazards that clear both HazardThreshold and
// HazardFloor, in rank order. scores must come from RankHazards.
func FlaggedHazards(scores []HazardScore) []HazardScore {
	var flagged []HazardScore
	for _, s := range scores {
		if s.Z >= HazardThreshold && s.Probability >= HazardFloor {
			flagged = append(flagged, s)
		}
	}
	return flagged
}

// reviewDigest identifies the hazard questions and review state template the
// review baseline was measured with.
func reviewDigest() string {
	return digestPayload(map[string]any{"context": reviewContext, "questions": ReviewQuestions()})
}
