package jev

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

// FixKind is one mechanism that fixes a cause. Several causes are fixed in
// more than one way, and the generic remedy names them all, so the optimizer
// chooses; for two of them Jev can tell which one a function needs.
type FixKind string

const (
	KindBuilder      FixKind = "sb_builder"
	KindDropFmt      FixKind = "sb_drop_fmt"
	KindASCII        FixKind = "fp_ascii"
	KindCommonCase   FixKind = "fp_common_case"
	KindComputeOnce  FixKind = "rd_compute_once"
	KindRemoveUnused FixKind = "rd_remove_unneeded"
	KindConversion   FixKind = "al_conversion"
	KindStackScratch FixKind = "al_stack_scratch"
	KindSizeHint     FixKind = "al_size_hint"
)

// FixKinds is every fix-kind question, in the order they are asked. All nine
// go in one request, as measured, but only the allocation and string-building
// kinds are chosen. The redundant-work pair picked the real fix 7 times in 12
// against 6 for always guessing, and no gate lifted it above chance. The
// fast-path pair did well on the fixes it was written from and badly on 15
// held-out ones: right 8 of the 11 times the gate let it choose, against 14 of
// 15 for always guessing the common-case kind, because the ASCII question
// fires on any function that scans bytes.
var FixKinds = []FixKind{KindBuilder, KindDropFmt, KindASCII, KindCommonCase, KindComputeOnce, KindRemoveUnused, KindConversion, KindStackScratch, KindSizeHint}

type kindSpec struct {
	cause    Cause
	question Question
	remedy   string
	chosen   bool
}

var kindSpecs = map[FixKind]kindSpec{
	KindBuilder: {CauseStringBuild, boolean(
		"Does the Go function in `source` assemble a string from pieces by concatenating with + or +=, or through a bytes.Buffer whose contents are converted to a string at the end, where writing the pieces into one strings.Builder or byte slice would avoid intermediate copies?",
		"Pieces are joined by repeated concatenation, or collected in a bytes.Buffer only to be turned into a string, and a single strings.Builder or byte slice would remove the extra copies.",
		"The function already writes into one strings.Builder or byte slice, or it does not build a string from several pieces."),
		"Write the pieces of the string in %s (%s) into one strings.Builder or byte slice instead of concatenating them or converting a bytes.Buffer.", true},
	KindDropFmt: {CauseStringBuild, boolean(
		"Does the Go function in `source` call fmt (Sprintf, Sprint, Fprintf, Errorf) to format values that strconv, plain concatenation, or the value itself would produce identically?",
		"A fmt call formats simple values, such as one number, one string, or a fixed pattern, that strconv, + or the value itself would give without reflection-based formatting.",
		"The function makes no fmt call, or its fmt calls format composite output that genuinely needs fmt's verbs."),
		"Replace the fmt formatting of simple values in %s (%s) with strconv calls, plain concatenation, or the value itself.", true},
	KindASCII: {CauseFastPath, boolean(
		"Does the Go function in `source` decode runes, consult unicode tables, or call a general rune or byte-set function on every character, when a byte-level test for ASCII input could handle the common case first?",
		"Each character pays for rune decoding, unicode lookup, or a general character-set search that a plain byte comparison would answer for ASCII input.",
		"The function already tests bytes directly for ASCII first, or it does not examine text character by character."),
		"Add a byte-level ASCII check in %s (%s) that handles plain ASCII input before any rune decoding or unicode lookup.", false},
	KindCommonCase: {CauseFastPath, boolean(
		"Does the Go function in `source` run its full general logic for an input that is common and trivially answered, such as identical operands, an empty or very small input, or a value that fits a simple form, where an early check could return the answer directly?",
		"A common easy input goes through the whole general computation although a cheap test at the start could return its result at once.",
		"Easy inputs are already answered by an early check, or every input needs the general computation."),
		"Add an early check in %s (%s) that returns the result for the common easy input before the general computation.", false},
	KindComputeOnce: {CauseRedundant, boolean(
		"Does the Go function in `source` compute the same value more than once, such as calling the same function with the same arguments twice, rescanning data it has already scanned, or creating an identical value on every call that could be a package-level variable?",
		"The same result is derived again, within one call or on every call, where computing it once and reusing it would give the same outcome.",
		"Each value is computed once, or its inputs genuinely differ each time."),
		"", false},
	KindRemoveUnused: {CauseRedundant, boolean(
		"Does the Go function in `source` do work whose result is never needed, such as a check that always passes, a copy or clear that is overwritten or unused, or an extra call or lookup whose result does not affect the outcome?",
		"Some statement or call can be deleted without changing what the function returns or does.",
		"Every statement contributes to the function's result or effects."),
		"", false},
	KindConversion: {CauseAlloc, boolean(
		"Does the Go function in `source` allocate through a conversion or an allocating call where a non-allocating equivalent exists, such as a string([]byte) or []byte(string) conversion, strings.ToLower before a comparison, boxing a value into an interface, or a Fields or Split call made only to iterate?",
		"An allocation comes from a conversion, an interface boxing, or an allocating library call that a non-allocating form (direct indexing, EqualFold, an iterator, a concrete type) would avoid.",
		"The function makes no such conversion or allocating call, or it already uses the non-allocating form."),
		"Remove the allocating conversion or call in %s (%s): index the bytes or string directly, use the non-allocating form (EqualFold, an iterator, a concrete type), or avoid boxing the value into an interface.", true},
	KindStackScratch: {CauseAlloc, boolean(
		"Does the Go function in `source` allocate a temporary slice or buffer with make or new on every call when its size is small or bounded, so that a fixed-size array on the stack, or a scratch buffer kept in the receiver, could hold it instead?",
		"A per-call make or new creates short-lived scratch space of small or bounded size that a local array or a reused field could provide without a heap allocation.",
		"The function allocates no temporary scratch space, its size is unbounded, or it already uses a local array or reused buffer."),
		"Hold the small per-call scratch buffer in %s (%s) in a fixed-size local array, or a buffer kept in the receiver, instead of allocating it on every call.", true},
	KindSizeHint: {CauseAlloc, boolean(
		"Does the Go function in `source` grow a slice, strings.Builder, or buffer by appending when its final size could be computed before the first append, so allocating it once at that size would avoid the intermediate reallocations?",
		"A container grows through appends or writes whose final length is known or cheaply computed up front, and no capacity or Grow call is given.",
		"The container is sized up front, its final size is unknowable, or nothing grows."),
		"Size the slice, builder, or buffer that %s (%s) grows up front from its known final length, with one make or Grow call.", true},
}

// kindBaseline is Jev's typical answer to each fix-kind question over the
// same 186 functions and state template as the cause baseline, measured
// 2026-09-24. On the benchmark's labelled fixes, the z-ranked kind named the
// real fix for 16 of 21 allocation fixes (always guessing the commonest kind:
// 9), 8 of 9 fast paths (6) and 8 of 12 string-building fixes (8); behind
// KindGate it was right 14 of 18, 8 of 8 and 7 of 8 times it chose. The
// questions were written after reading those fixes, so a held-out set of 60
// labelled fixes from 28 other repositories, labelled before any question was
// asked, re-scored them unchanged: behind the gate allocation was right 16 of
// 17 times (always guessing: 9 of 21) and string building 20 of 24 (13 of 24).
var kindBaseline = map[FixKind]stats{
	KindBuilder:      {mean: 0.1789, std: 0.2636},
	KindDropFmt:      {mean: 0.1336, std: 0.2293},
	KindASCII:        {mean: 0.1183, std: 0.1593},
	KindCommonCase:   {mean: 0.4068, std: 0.1959},
	KindComputeOnce:  {mean: 0.3286, std: 0.1796},
	KindRemoveUnused: {mean: 0.3623, std: 0.1520},
	KindConversion:   {mean: 0.2909, std: 0.2101},
	KindStackScratch: {mean: 0.2531, std: 0.2017},
	KindSizeHint:     {mean: 0.2102, std: 0.2093},
}

const kindBaselineDigest = "e93b226ef549138c95a626a2640fae471a58a2c01de2bd2283ddd0aefb274b2d"

// KindGate is how far the chosen kind must stand: at or above its usual
// answer, and this many standard deviations ahead of the cause's next kind.
// Below it the cause's generic remedy stands, as before.
const KindGate = 0.25

// KindQuestions returns every fix-kind question, keyed by kind.
func KindQuestions() map[string]Question {
	questions := make(map[string]Question, len(FixKinds))
	for _, k := range FixKinds {
		questions[string(k)] = kindSpecs[k].question
	}
	return questions
}

// HasKinds reports whether Jev chooses among fix kinds for the cause.
func HasKinds(c Cause) bool {
	for _, k := range FixKinds {
		if s := kindSpecs[k]; s.cause == c && s.chosen {
			return true
		}
	}
	return false
}

// Remedy is the optimizer hypothesis for this fix kind at one site.
func (k FixKind) Remedy(function, location string) string {
	return fmt.Sprintf(kindSpecs[k].remedy, function, location)
}

// KindScore is one fix kind's answer for one site.
type KindScore struct {
	Kind        FixKind `json:"kind"`
	Probability float64 `json:"probability"`
	Z           float64 `json:"z"`
}

// ChooseKind ranks the cause's kinds by z and returns the leader when it
// clears KindGate, or false to keep the generic remedy.
func ChooseKind(c Cause, answers map[string]Answer) (FixKind, []KindScore, bool, error) {
	scores, err := kindScores(c, answers)
	if err != nil {
		return "", nil, false, err
	}
	slices.SortStableFunc(scores, func(a, b KindScore) int { return cmp.Compare(b.Z, a.Z) })
	if len(scores) < 2 || scores[0].Z < 0 || scores[0].Z-scores[1].Z < KindGate {
		return "", scores, false, nil
	}
	return scores[0].Kind, scores, true, nil
}

// kindScores scores each kind Jev may choose for the cause against its
// baseline.
func kindScores(c Cause, answers map[string]Answer) ([]KindScore, error) {
	var scores []KindScore
	for _, k := range FixKinds {
		if s := kindSpecs[k]; s.cause != c || !s.chosen {
			continue
		}
		answer, ok := answers[string(k)]
		if !ok {
			return nil, fmt.Errorf("no answer to the %s question", k)
		}
		p := answer.Probability
		if math.IsNaN(p) || p < 0 || p > 1 {
			return nil, fmt.Errorf("answer to the %s question has probability %v outside [0, 1]", k, p)
		}
		b := kindBaseline[k]
		scores = append(scores, KindScore{Kind: k, Probability: p, Z: (p - b.mean) / b.std})
	}
	return scores, nil
}

func kindDigest() string {
	payload, err := json.Marshal(map[string]any{"context": stateContext, "questions": KindQuestions()})
	if err != nil {
		panic(err) // static strings always marshal
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
