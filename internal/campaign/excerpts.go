package campaign

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
)

const (
	maxExcerptLines      = 120
	excerptContextBefore = 40
	maxExcerptBytes      = 8 * 1024
	defaultMaxExcerpts   = 32 * 1024
)

// extractExcerpts reads real source around analyst-identified hot paths so
// optimizer patches can carry context lines that git apply accepts. It is
// deterministic and best-effort: unusable locations are skipped silently.
func extractExcerpts(repoRoot string, hotPaths []agents.HotPath, maxTotal int) ([]orchestrator.SourceExcerpt, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("repository root is required")
	}
	if maxTotal <= 0 {
		maxTotal = defaultMaxExcerpts
	}
	limit := len(hotPaths)
	if limit > 5 {
		limit = 5
	}
	var excerpts []orchestrator.SourceExcerpt
	total := 0
	for _, hp := range hotPaths[:limit] {
		path, line, ok := parseLocation(hp.Location)
		if !ok {
			continue
		}
		full := filepath.Join(repoRoot, path)
		content, start, size := readWindow(full, line)
		if content == "" {
			continue
		}
		if total+size > maxTotal {
			// Later hot paths are lower priority; stop rather than truncate
			// an excerpt to a misleading fragment.
			break
		}
		excerpts = append(excerpts, orchestrator.SourceExcerpt{
			Path:      filepath.ToSlash(path),
			StartLine: start,
			Content:   content,
			HotPath:   hp.Location,
		})
		total += size
	}
	return excerpts, nil
}

// parseLocation splits "path.go:123" or "path.go", rejecting absolute paths
// and anything escaping the repository root.
func parseLocation(location string) (string, int, bool) {
	loc := strings.TrimSpace(location)
	if loc == "" {
		return "", 0, false
	}
	line := 0
	if idx := strings.LastIndex(loc, ":"); idx > 0 {
		if n, err := strconv.Atoi(loc[idx+1:]); err == nil && n > 0 {
			loc, line = loc[:idx], n
		}
	}
	if filepath.IsAbs(loc) || strings.HasPrefix(loc, "..") {
		return "", 0, false
	}
	clean := filepath.Clean(loc)
	if clean == "." || strings.HasPrefix(clean, "..") {
		return "", 0, false
	}
	return clean, line, true
}

// readWindow returns up to maxExcerptLines of the file ending excerptContextBefore
// lines before the target (or from the file start), capped at maxExcerptBytes.
func readWindow(path string, line int) (string, int, int) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "", 0, 0
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	end := len(lines)
	start := 0
	if line > 0 {
		// Window begins excerptContextBefore lines before the hot line.
		start = max(line-1-excerptContextBefore, 0)
		end = min(start+maxExcerptLines, len(lines))
	}
	window := lines[start:end]
	var b strings.Builder
	for i, l := range window {
		if b.Len()+len(l)+1 > maxExcerptBytes {
			window = window[:i]
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	if len(window) == 0 {
		return "", 0, 0
	}
	return b.String(), start + 1, b.Len()
}
