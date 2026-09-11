// Package candidate: unified-diff normalization for model-proposed patches.
package candidate

import (
	"fmt"
	"strings"
)

// NormalizeUnifiedDiff repairs common defects in model-emitted unified diffs:
// incorrect hunk line counts and trailing garbage inside hunk bodies. It
// returns the normalized diff and whether anything changed.
func NormalizeUnifiedDiff(patch string) (string, bool) {
	if !strings.Contains(patch, "@@") {
		return patch, false
	}
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")

	var out []string
	headersIdx := -1
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "@@ ") {
			out, headersIdx, i = appendNonHunk(out, lines, i, headersIdx)
			continue
		}
		header := lines[i]
		body, next := collectHunkBody(lines, i+1)
		i = next
		out, headersIdx = applyNormalizedHunk(out, headersIdx, header, body)
	}

	// The canonical form always ends with exactly one trailing newline.
	result := strings.Join(out, "\n") + "\n"
	return result, result != patch
}

func appendNonHunk(out, lines []string, i, headersIdx int) ([]string, int, int) {
	line := lines[i]
	// Models wrap the diff in a Markdown fence inside the patch value. The
	// closing fence lands in a hunk body and is dropped there as garbage, but
	// the opening one sits in the preamble and would reach git apply.
	if strings.HasPrefix(line, "```") {
		return out, headersIdx, i + 1
	}
	out = append(out, line)
	if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
		out = append(out, lines[i+1])
		return out, len(out) - 2, i + 2
	}
	return out, headersIdx, i + 1
}

func collectHunkBody(lines []string, i int) ([]string, int) {
	var body []string
	for i < len(lines) {
		l := lines[i]
		if strings.HasPrefix(l, "@@ ") || strings.HasPrefix(l, "--- ") {
			break
		}
		body = append(body, l)
		i++
	}
	return body, i
}

func applyNormalizedHunk(out []string, headersIdx int, header string, body []string) ([]string, int) {
	if norm := normalizeHunk(header, body); norm != nil {
		return append(out, norm...), -1
	}
	if headersIdx >= 0 {
		// Drop the file-section headers of an emptied hunk.
		return append(out[:headersIdx], nil...), -1
	}
	return out, headersIdx
}

// normalizeHunk truncates a hunk at its first malformed body line and rewrites
// its @@ header with corrected line counts. Returns nil if the hunk becomes
// empty and should be dropped.
func normalizeHunk(header string, body []string) []string {
	kept := keepHunkLines(body)
	oldCount, newCount := countHunkLines(kept)
	if oldCount == 0 && newCount == 0 {
		return nil
	}
	oldStart, newStart := parseStarts(header)
	newHeader := fmt.Sprintf("@@ -%s,%d +%s,%d @@", oldStart, oldCount, newStart, newCount)
	newHeader += hunkHeaderTrail(header)
	out := make([]string, 0, len(kept)+1)
	out = append(out, newHeader)
	out = append(out, kept...)
	return out
}

// keepHunkLines returns the leading run of body lines that belong to the hunk,
// restoring blank context lines on the way.
//
// An empty line is ambiguous. A unified diff writes a blank source line as a
// single space, and that lone trailing space is the first thing lost whenever a
// diff passes through a model, a JSON string, or anything that trims line ends.
// Ending the hunk at every empty line therefore truncated genuine patches at
// their first blank source line, and when the blank fell on the first body line
// the hunk emptied out, its file headers were dropped with it, and the campaign
// rejected the candidate as a malformed unified diff. A hunk body is
// contiguous, so an empty run with more body lines after it can only be blank
// context; an empty run at the end is trailing slack and still ends the hunk.
func keepHunkLines(body []string) []string {
	kept := make([]string, 0, len(body))
	for i := 0; i < len(body); {
		if body[i] == "" {
			run, next := blankContextRun(body, i)
			if run == nil {
				return kept
			}
			kept = append(kept, run...)
			i = next
			continue
		}
		if !isHunkBodyPrefix(body[i][0]) {
			return kept
		}
		kept = append(kept, body[i])
		i++
	}
	return kept
}

// blankContextRun rewrites the run of empty lines starting at i as context
// lines, returning nil when nothing belonging to the hunk follows the run.
func blankContextRun(body []string, i int) ([]string, int) {
	end := i
	for end < len(body) && body[end] == "" {
		end++
	}
	if end >= len(body) || !isHunkBodyPrefix(body[end][0]) {
		return nil, end
	}
	run := make([]string, end-i)
	for j := range run {
		run[j] = " "
	}
	return run, end
}

func isHunkBodyPrefix(c byte) bool {
	return c == ' ' || c == '+' || c == '-' || c == '\\'
}

func countHunkLines(kept []string) (oldCount, newCount int) {
	for _, l := range kept {
		switch l[0] {
		case ' ':
			oldCount++
			newCount++
		case '-':
			oldCount++
		case '+':
			newCount++
		}
	}
	return oldCount, newCount
}

func hunkHeaderTrail(header string) string {
	if idx := strings.Index(strings.TrimPrefix(header, "@@ "), "@@"); idx >= 0 {
		if trail := strings.TrimPrefix(header[3+idx+2:], ""); trail != "" {
			return trail
		}
	}
	return ""
}

// parseStarts extracts the declared old/new start lines from a @@ header,
// defaulting to 1 when absent or implausible (non-numeric).
func parseStarts(header string) (oldStart, newStart string) {
	fields := strings.Fields(header)
	oldStart, newStart = "1", "1"
	for _, f := range fields {
		s := f
		isOld := strings.HasPrefix(s, "-")
		isNew := !isOld && strings.HasPrefix(s, "+")
		if !isOld && !isNew {
			continue
		}
		s = s[1:]
		if j := strings.Index(s, ","); j >= 0 {
			s = s[:j]
		}
		if s == "" || strToPosInt(s) <= 0 {
			continue
		}
		if isOld {
			oldStart = s
		} else {
			newStart = s
		}
	}
	return oldStart, newStart
}

func strToPosInt(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
