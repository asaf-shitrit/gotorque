package discovery

import (
	"bytes"
	"errors"
	"slices"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// retryAmplificationTarget is the input size of the one retry for a target
// that finished before the sampler could attach.
const retryAmplificationTarget = 8 * amplificationTarget

// retriesLarger reports whether a failed sample is worth one more try on a
// larger input: the target ended too soon, or the sampler caught it before it
// was doing anything, and its inputs can grow.
func retriesLarger(err error, seed manifest.SeedWorkload) bool {
	tooEarly := errors.Is(err, profile.ErrTargetExitedEarly) || errors.Is(err, profile.ErrNoFrames) || errors.Is(err, profile.ErrIdle)
	return tooEarly && hasRepeatableInput(seed)
}

func hasRepeatableInput(seed manifest.SeedWorkload) bool {
	return seed.StdinRepeat > 0 || slices.ContainsFunc(seed.Files, func(f manifest.FixtureFile) bool { return f.Repeat > 0 })
}

// amplifyRepeats scales every input the manifest declares repeatable -- a
// fixture file with repeat, stdin with stdin_repeat -- so that input expands
// to about amplificationTarget bytes for the sampled run. The declaration is
// what makes this safe: the manifest author wrote the content as a block that
// may be written any number of times. Only stdin was amplified before, so a
// CLI that reads files ran for milliseconds, the sampler could not attach, and
// discovery fell back to benchmarks: every held-out target reads files.
// Inputs without a declared repeat (a script, a single document) are left as
// they are; a manifest keeps those sampleable with a long stress seed.
func amplifyRepeats(seed manifest.SeedWorkload, target int) manifest.SeedWorkload {
	if seed.StdinRepeat > 0 {
		seed.StdinRepeat = scaledRepeat(len(seed.StdinHeader), len(seed.Stdin), seed.StdinRepeat, target)
	}
	files := make([]manifest.FixtureFile, len(seed.Files))
	for i, f := range seed.Files {
		if f.Repeat > 0 {
			f.Repeat = scaledRepeat(len(f.Header), len(f.Content), f.Repeat, target)
		}
		files[i] = f
	}
	seed.Files = files
	return seed
}

// scaledRepeat is the repeat count that brings header plus block to about
// target bytes, never fewer than the manifest's own count.
func scaledRepeat(header, block, repeat, target int) int {
	if block <= 0 {
		return repeat
	}
	return max(repeat, (target-header)/block)
}

// amplifyStdin grows a seed input so a short-lived target stays alive for the
// sampler's window.
//
// Repeating the raw bytes only lengthens the run for a target that consumes
// all of stdin. A single-shot JSON CLI reads one document and ignores the
// rest: gron finished a 16 MiB concatenation of its 84 KiB seed in 21 ms,
// exactly as fast as the unamplified seed, so the target was gone before the
// sampler could attach and every campaign fell back to a benchmark profile.
// Replicating the elements of the document's largest array keeps the document
// valid and multiplies the work it describes, which turns that same seed into
// a multi-second run.
func amplifyStdin(stdin []byte) []byte {
	if len(stdin) == 0 || len(stdin) >= maxAmplifiedStdin {
		return stdin
	}
	if amplified, ok := amplifyJSONArray(stdin); ok {
		return amplified
	}
	return repeatStdin(stdin)
}

// maxAmplifiedStdin bounds a sampling input's size.
const maxAmplifiedStdin = 32 << 20

// amplificationTarget is how much input the amplifiers aim to produce.
const amplificationTarget = 16 << 20

// amplifyJSONArray duplicates the body of the largest JSON array so the result
// is still one valid document. It reports false when stdin is not JSON with a
// non-empty array, leaving the caller to fall back to byte repetition.
func amplifyJSONArray(stdin []byte) ([]byte, bool) {
	start, end, ok := largestJSONArray(stdin)
	if !ok {
		return nil, false
	}
	body := stdin[start+1 : end]
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	// The prefix already ends with '[', so each repetition contributes a
	// separating comma and one more element list.
	amplified := make([]byte, 0, amplificationTarget)
	amplified = append(amplified, stdin[:end]...)
	for len(amplified) < amplificationTarget {
		amplified = append(amplified, ',')
		amplified = append(amplified, body...)
	}
	return append(amplified, stdin[end:]...), true
}

// jsonSpan is a half-open byte range of a JSON container.
type jsonSpan struct{ start, end int }

// largestJSONArray returns the offsets of the '[' and ']' of the largest JSON
// array in data. The scan is string-aware so brackets inside string literals
// cannot confuse it, and it finds nested arrays because every closing bracket
// is compared against the opening one it matches.
func largestJSONArray(data []byte) (start, end int, ok bool) {
	var stack []int
	best := jsonSpan{start: -1, end: -1}
	var state jsonScanState
	for i := 0; i < len(data); i++ {
		if state.advance(data[i]) {
			continue
		}
		switch data[i] {
		case '[':
			stack = append(stack, i)
		case ']':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			best = widest(best, jsonSpan{start: open, end: i})
		}
	}
	if best.start < 0 {
		return 0, 0, false
	}
	return best.start, best.end, true
}

// jsonScanState tracks whether the scan is inside a string literal, so a
// bracket in string content is never mistaken for structure.
type jsonScanState struct {
	inString bool
	escaped  bool
}

// advance consumes one byte and reports whether it belonged to a string
// literal, meaning the caller must not treat it as structure.
func (s *jsonScanState) advance(c byte) bool {
	if !s.inString {
		if c == '"' {
			s.inString = true
			return true
		}
		return false
	}
	switch {
	case s.escaped:
		s.escaped = false
	case c == '\\':
		s.escaped = true
	case c == '"':
		s.inString = false
	}
	return true
}

func widest(a, b jsonSpan) jsonSpan {
	if b.end-b.start > a.end-a.start {
		return b
	}
	return a
}

// repeatLines repeats the input one copy per line, the shape a line-oriented
// mode reads as many documents.
func repeatLines(stdin []byte) []byte {
	return repeatStdin(append(bytes.TrimRight(stdin, "\n"), '\n'))
}

// repeatStdin is the format-agnostic fallback: it lengthens the input for any
// target that consumes all of stdin.
func repeatStdin(stdin []byte) []byte {
	amplified := make([]byte, 0, maxAmplifiedStdin)
	for len(amplified) < amplificationTarget {
		amplified = append(amplified, stdin...)
	}
	return amplified
}
