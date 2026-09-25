package jev

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Cause names one mechanism that makes a Go function slower than it needs to
// be. The set mirrors the analyst prompt's cause model.
type Cause string

const (
	CauseAlloc        Cause = "alloc"
	CauseUnbufferedIO Cause = "unbuffered_io"
	CauseStringBuild  Cause = "string_build"
	CauseFastPath     Cause = "fast_path"
	CauseSuperlinear  Cause = "superlinear"
	CausePrealloc     Cause = "prealloc"
	CauseRedundant    Cause = "redundant"
)

// Causes is the stable order used for questions, ties, and reports.
var Causes = []Cause{CauseAlloc, CauseUnbufferedIO, CauseStringBuild, CauseFastPath, CauseSuperlinear, CausePrealloc, CauseRedundant}

// IsAllocation reports whether a cause names a mechanism that grows the heap:
// an unnecessary allocation, a slice or map grown without a size hint, or a
// string built by repeated concatenation. A campaign targeting peak memory
// ranks these causes first; every other cause (unbuffered I/O, a slow path,
// superlinear work, redundant computation) can win time without moving
// memory at all.
func (c Cause) IsAllocation() bool {
	return c == CauseAlloc || c == CausePrealloc || c == CauseStringBuild
}

// stateContext opens every state. It is part of what the baseline was measured
// against, together with the field names in SiteState and the question text.
const stateContext = "This Go function appears among the CPU-hot functions of a profiled run of its program."

type causeSpec struct {
	question Question
	// summary names the cause in likely_causes and evidence lines.
	summary string
	// remedy is the one-sentence hypothesis handed to the optimizer, formatted
	// with the function name and its location.
	remedy string
}

// One yes/no question per cause, never a single "which cause" choice. The
// benchmark behind this package (93 real single-function performance fixes,
// each asked about before and after the fix) found the choice question the
// weakest way to name a cause, and the only one a misleading profile in the
// state could derail: its accuracy halved while the yes/no answers did not
// move. Question ids never reach the model, so every instruction carries its
// full meaning, and every instruction names the state field it is about
// (`source`), as TypeSafe's guidance asks because Jev reads literally.
//
// Three questions deliberately judge a relationship rather than a single fact
// (allocates per call and could avoid it; a general path taken and a cheap
// check possible; grows incrementally and the size is known). Splitting them
// into one fact per question and multiplying the answers was tried and lost a
// fifth of the fix detection: each part describes something that survives the
// fix — a function still appends after gaining a capacity hint, and its final
// size becomes more obviously known, not less — so only the relationship
// separates broken code from fixed code. Structured criteria with examples on
// both sides were tried too and gave no net gain.
var specs = map[Cause]causeSpec{
	CauseAlloc: {
		question: boolean(
			"Does the Go function in `source` make a heap allocation on every call or every loop iteration that a small change could avoid?",
			"An avoidable allocation happens per call or per iteration: a temporary buffer, slice, map, closure, boxed interface value, pointer that escapes, or a string/[]byte conversion.",
			"Allocations are absent, unavoidable, or already amortized, for example reused buffers or stack-allocated values."),
		summary: "avoidable per-call heap allocation",
		remedy:  "Remove the avoidable per-call heap allocation in %s (%s) by reusing a buffer, dropping a conversion, or keeping the value on the stack.",
	},
	CauseUnbufferedIO: {
		question: boolean(
			"Does the Go function in `source` perform many small reads or writes directly on a file, pipe, network connection, or stdout, without a buffer in between?",
			"Each record, line, or small chunk becomes its own read or write call on an unbuffered reader or writer.",
			"I/O is absent, already buffered (bufio or an in-memory buffer), or done in large chunks."),
		summary: "many small unbuffered reads or writes",
		remedy:  "Route the small reads or writes in %s (%s) through a bufio reader or writer, flushed once, so each record is not its own system call.",
	},
	CauseStringBuild: {
		question: boolean(
			"Does the Go function in `source` build strings inefficiently, such as concatenating in a loop, using fmt.Sprintf where strconv or direct appends would do, or converting between string and []byte repeatedly?",
			"String construction or formatting does avoidable copying or reflection-based formatting.",
			"Strings are built with a Builder, direct appends into a byte slice, or not built at all."),
		summary: "inefficient string building or formatting",
		remedy:  "Build the strings in %s (%s) with strings.Builder, strconv, or direct appends instead of concatenation or fmt.Sprintf.",
	},
	CauseFastPath: {
		question: boolean(
			"In the Go function in `source`, does every input go through a general, expensive path (unicode tables, reflection, generic decoding, full parsing) when a cheap check could handle the common case directly?",
			"A common, simple case such as ASCII input, a small value, or an exact type pays for the general-case machinery.",
			"The common case is already special-cased, or there is no costly general path."),
		summary: "common case pays for the general path",
		remedy:  "Add a cheap check for the common case in %s (%s) so simple inputs skip the general, expensive path.",
	},
	CauseSuperlinear: {
		question: boolean(
			"Does the cost of the Go function in `source` grow faster than linearly with its input, for example quadratic nested loops, repeated scans of the same data, or re-sorting inside a loop?",
			"Some input can make the function do quadratic or worse work where a linear or n log n approach exists.",
			"The work is linear or already uses an appropriate algorithm."),
		summary: "superlinear work over its input",
		remedy:  "Replace the repeated scan or nested loop over the same data in %s (%s) with a single pass or an index.",
	},
	CausePrealloc: {
		question: boolean(
			"Does the Go function in `source` grow a slice, map, or buffer incrementally when its final size is known or cheaply computable in advance?",
			"Appends or inserts trigger repeated growth that a capacity or size hint would prevent.",
			"Containers are sized up front, their size is unknowable, or no container grows."),
		summary: "container grown without a known size hint",
		remedy:  "Size the growing slice, map, or buffer in %s (%s) up front from its known final length.",
	},
	CauseRedundant: {
		question: boolean(
			"Does the Go function in `source` repeat work whose result does not change, such as recomputing a loop-invariant value, repeating a lookup or check, or recalculating something that could be computed once?",
			"The same computation, lookup, or validation is performed more than once with the same result.",
			"Each computation happens once or its inputs genuinely change."),
		summary: "repeated work whose result does not change",
		remedy:  "Hoist or cache the repeated computation in %s (%s) so it runs once.",
	},
}

func boolean(instructions, whenTrue, whenFalse string) Question {
	return Question{Type: "boolean", Instructions: instructions, Criteria: map[string]string{"true": whenTrue, "false": whenFalse}}
}

// Questions returns the question set asked about every hot function, keyed by
// cause.
func Questions() map[string]Question {
	questions := make(map[string]Question, len(Causes))
	for _, cause := range Causes {
		questions[string(cause)] = specs[cause].question
	}
	return questions
}

// SiteState is the state for one hot function: its file and its whole source.
//
// It deliberately carries no profile. The first probe gave Jev the gron
// benchmark profile alongside the source and it labelled an unbuffered
// Fprintln loop and a per-call bytes.Buffer as "lookup" cost, because that
// profile was dominated by unicode-table lookups; without it both were named
// correctly. The profile already did its job by choosing which functions to
// ask about.
func SiteState(file, source string) map[string]string {
	return map[string]string{"context": stateContext, "file": file, "source": source}
}

func (c Cause) Summary() string { return specs[c].summary }

// Remedy is the optimizer hypothesis for this cause at one site.
func (c Cause) Remedy(function, location string) string {
	return fmt.Sprintf(specs[c].remedy, function, location)
}

// Score is one cause's answer for one site.
type Score struct {
	Cause       Cause   `json:"cause"`
	Probability float64 `json:"probability"`
	// Z is the answer's distance from Jev's typical answer to the same
	// question, in baseline standard deviations.
	Z float64 `json:"z"`
}

// Rank orders a site's causes by how far each answer sits above Jev's usual
// answer to that question.
//
// Raw probabilities are not comparable across questions: each question is
// calibrated on its own, and the allocation question says yes to most Go
// functions, so the highest raw answer named the right cause only 36% of the
// time in the benchmark. Measured against each question's own baseline the
// same answers named it 50% of the time and put it in the top two 69% of the
// time (chance is 14%), with no labels involved. Ties keep the Causes order so
// the ranking is reproducible.
func Rank(answers map[string]Answer) ([]Score, error) {
	scores := make([]Score, 0, len(Causes))
	for _, cause := range Causes {
		answer, ok := answers[string(cause)]
		if !ok {
			return nil, fmt.Errorf("no answer to the %s question", cause)
		}
		p := answer.Probability
		if math.IsNaN(p) || p < 0 || p > 1 {
			return nil, fmt.Errorf("answer to the %s question has probability %v outside [0, 1]", cause, p)
		}
		b := baseline[cause]
		scores = append(scores, Score{Cause: cause, Probability: p, Z: (p - b.mean) / b.std})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Z > scores[j].Z })
	return scores, nil
}

// FlagThreshold and MaxFlagged decide which ranked causes a site reports. On
// the benchmark, half a standard deviation and at most two causes put the
// fixing commit's cause among the flags for 59% of unfixed functions while
// still flagging it on only 23% of the fixed versions, and left 26 of 93 fixed
// functions with no flag at all against 13 of 93 unfixed. A zero threshold
// raised the first figure to 63% but flagged a third of the fixed code.
//
// ProbabilityFloor is the second half of the gate (ADR 0025): a cause is
// flagged only when Jev also says yes outright. z alone does not separate the
// targets that led to accepted fixes from the ones that did not: over 38
// targets attempted in live campaigns, every accepted fix had p >= 0.67 and
// none of the 14 targets below 0.5 was accepted, while the highest z-scores in
// the record, all fast_path, failed. On the held-out fixes the floor cut flags
// on already-fixed code from 30% to 20% of labelled causes, and actions on
// fixed functions from 78% to 47%, with recall held outside fast_path.
const (
	FlagThreshold    = 0.5
	ProbabilityFloor = 0.5
	MaxFlagged       = 2
)

// Deferred reports a cause gotorque still flags but attacks only after every
// other target (ADR 0025). fast_path is the one: 17 fast-path targets were
// attempted in live campaigns and none was accepted, including 8 Jev answered
// yes to, because the optimizer turns the flag into branches that rarely fire.
func Deferred(c Cause) bool { return c == CauseFastPath }

// Flag keeps the causes that clear both gates, in z order, skipping any the
// caller excludes: at most MaxFlagged that are not Deferred, plus a Deferred
// one when it clears the gates. An ineligible cause is skipped, not a stop, so
// a lower cause that clears the gates still gets its slot. scores must come
// from Rank.
func Flag(scores []Score, excluded func(Cause) bool) []Score {
	var flagged []Score
	primary := 0
	for _, s := range scores {
		if s.Z < FlagThreshold {
			break
		}
		if s.Probability < ProbabilityFloor || (excluded != nil && excluded(s.Cause)) {
			continue
		}
		if Deferred(s.Cause) {
			flagged = append(flagged, s)
			continue
		}
		if primary < MaxFlagged {
			flagged = append(flagged, s)
			primary++
		}
	}
	return flagged
}

// Flagged is Flag with nothing excluded.
func Flagged(scores []Score) []Score { return Flag(scores, nil) }

type stats struct{ mean, std float64 }

// digest identifies the question set and state template. The baseline below is
// only valid for the exact text it was measured with.
func digest() string {
	payload, err := json.Marshal(map[string]any{"context": stateContext, "questions": Questions()})
	if err != nil {
		panic(err) // static strings always marshal
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
