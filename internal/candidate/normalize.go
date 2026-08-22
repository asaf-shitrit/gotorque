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
		line := lines[i]
		if !strings.HasPrefix(line, "@@ ") {
			out = append(out, line)
			// Keep "--- "/"+++ " header pairs verbatim.
			if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
				headersIdx = len(out) - 1
				i++
				out = append(out, lines[i])
			}
			i++
			continue
		}

		var body []string
		i++
		for i < len(lines) {
			l := lines[i]
			if strings.HasPrefix(l, "@@ ") || strings.HasPrefix(l, "--- ") {
				break
			}
			body = append(body, l)
			i++
		}

		if norm := normalizeHunk(line, body); norm != nil {
			out = append(out, norm...)
			headersIdx = -1
		} else if headersIdx >= 0 {
			// Drop the file-section headers of an emptied hunk.
			out = append(out[:headersIdx], nil...)
			headersIdx = -1
		}
	}

	// The canonical form always ends with exactly one trailing newline.
	result := strings.Join(out, "\n") + "\n"
	return result, result != patch
}

// normalizeHunk truncates a hunk at its first malformed body line and rewrites
// its @@ header with corrected line counts. Returns nil if the hunk becomes
// empty and should be dropped.
func normalizeHunk(header string, body []string) []string {
	oldStart, newStart := parseStarts(header)

	kept := make([]string, 0, len(body))
	for _, l := range body {
		if len(l) == 0 {
			break // stripped leading space or stray blank line
		}
		c := l[0]
		if c != ' ' && c != '+' && c != '-' && c != '\\' {
			break
		}
		kept = append(kept, l)
	}

	oldCount, newCount := 0, 0
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

	if oldCount == 0 && newCount == 0 {
		return nil
	}

	newHeader := fmt.Sprintf("@@ -%s,%d +%s,%d @@", oldStart, oldCount, newStart, newCount)
	if idx := strings.Index(strings.TrimPrefix(header, "@@ "), "@@"); idx >= 0 {
		if trail := strings.TrimPrefix(header[3+idx+2:], ""); trail != "" {
			newHeader += trail
		}
	}
	out := make([]string, 0, len(kept)+1)
	out = append(out, newHeader)
	out = append(out, kept...)
	return out
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
