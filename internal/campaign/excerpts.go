package campaign

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	// Twelve windows, and a total budget that fits twelve full ones. Five was
	// the limiter the optimizer actually hit: discovery measures 15-29 hot
	// functions per target (gron: 25, gojq: 8) and the analyst adds its own,
	// so five windows showed the model roughly a fifth of the hot path and
	// each candidate could only attack the one frame it could see. Twelve
	// costs about 24k prompt tokens, which is noise against a model call that
	// already takes 15s-3m, and covers the top half of a measured hot list.
	maxExcerpts        = 12
	defaultMaxExcerpts = maxExcerpts * maxExcerptBytes
)

// extractExcerpts reads real source around analyst-identified hot paths so
// optimizer patches can carry context lines that git apply accepts. It is
// deterministic and best-effort: unusable locations are skipped silently.
func extractExcerpts(repoRoot string, hotPaths []agents.HotPath, maxTotal int) ([]orchestrator.SourceExcerpt, error) {
	if repoRoot == "" {
		return nil, errors.New("repository root is required")
	}
	if maxTotal <= 0 {
		maxTotal = defaultMaxExcerpts
	}
	var excerpts []orchestrator.SourceExcerpt
	total := 0
	seen := map[string]bool{}
	for _, hp := range hotPaths {
		if len(excerpts) == maxExcerpts {
			break
		}
		path, line, ok := parseLocation(hp.Location)
		if !ok {
			continue
		}
		// The cap counts excerpts actually produced, not locations examined:
		// capping candidates first meant five unusable leading locations
		// yielded nothing even when later ones resolved cleanly.
		key := fmt.Sprintf("%s:%d", path, line)
		if seen[key] {
			continue
		}
		seen[key] = true
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

// withFileHeaders puts each Go file's package clause and imports in front of
// the first window from that file that does not already show them, tagged
// with that window's hot path.
//
// A remedy often needs a new import, and the optimizer may only base hunks on
// the excerpts it is given. Shown only the function, it guessed the top of
// the file: on a live gojq campaign, told that its bufio fix lacked the
// import, it wrote an import hunk whose context assumed cli/cli.go opens with
// its package clause, and the file opens with a doc comment, so the patch did
// not apply.
func withFileHeaders(repoRoot string, excerpts []orchestrator.SourceExcerpt) []orchestrator.SourceExcerpt {
	out := make([]orchestrator.SourceExcerpt, 0, len(excerpts))
	done := map[string]bool{}
	for _, e := range excerpts {
		if !done[e.Path] && e.StartLine > 1 && strings.HasSuffix(e.Path, ".go") {
			if content := fileHeader(filepath.Join(repoRoot, filepath.FromSlash(e.Path))); content != "" {
				out = append(out, orchestrator.SourceExcerpt{Path: e.Path, StartLine: 1, Content: content, HotPath: e.HotPath})
			}
		}
		done[e.Path] = true
		out = append(out, e)
	}
	return out
}

// fileHeader is the file from its first line through its import declarations,
// or through the package clause when it imports nothing.
func fileHeader(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return ""
	}
	end := fset.Position(file.Name.End()).Line
	for _, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			end = fset.Position(gd.End()).Line
		}
	}
	lines := strings.Split(string(data), "\n")
	header := strings.Join(lines[:min(end, len(lines))], "\n")
	if len(header) > maxExcerptBytes {
		return ""
	}
	return header
}
