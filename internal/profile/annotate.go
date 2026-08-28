package profile

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HotLocation is one profiled function resolved to a repository-relative
// source position, precise enough for the excerpt pipeline to read real
// code instead of the optimizer guessing patch context.
type HotLocation struct {
	Function string `json:"function"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
}

// Location renders the position in the path:line form the excerpt
// collector and analyst prompts expect.
func (h HotLocation) Location() string {
	if h.Line > 0 {
		return h.Path + ":" + itoa(h.Line)
	}
	return h.Path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

var pprofLineMarker = regexp.MustCompile(`\(line (\d+)\)`)

var pprofListHeader = regexp.MustCompile(`^ROUTINE =+ (.+) in (.+)$`)

// ParsePprofList extracts the file and first sampled line from
// `go tool pprof -list <func>` output. The header carries the absolute or
// module-relative source path; content lines look like "  .  .  41:code".
// Returns ok=false when the output holds no recognizable routine.
func ParsePprofList(output string) (path string, line int, ok bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	inRoutine := false
	for scanner.Scan() {
		text := strings.TrimRight(scanner.Text(), "\r")
		if m := pprofListHeader.FindStringSubmatch(text); m != nil {
			path = strings.TrimSpace(m[2])
			inRoutine = true
			continue
		}
		if !inRoutine || path == "" {
			continue
		}
		lineNumber, stop := pprofListLineNumber(text)
		if stop {
			break
		}
		line = minSampleLine(line, lineNumber)
	}
	if path == "" {
		return "", 0, false
	}
	return path, line, true
}

func pprofListLineNumber(text string) (int, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, "ROUTINE ") {
		return 0, true
	}
	if m := pprofLineMarker.FindStringSubmatch(trimmed); m != nil {
		return atoi(m[1]), false
	}
	return colonLineNumber(trimmed), false
}

func colonLineNumber(trimmed string) int {
	colon := strings.Index(trimmed, ":")
	if colon <= 0 {
		return 0
	}
	lineNumber := 0
	for _, ch := range trimmed[:colon] {
		if ch < '0' || ch > '9' {
			return 0
		}
		lineNumber = lineNumber*10 + int(ch-'0')
	}
	return lineNumber
}

func minSampleLine(current, n int) int {
	if n > 0 && (current == 0 || n < current) {
		return n
	}
	return current
}

// FindFunctionInRepo locates a function declaration by name under root,
// searching .go files breadth-first with a bounded file count. It is the
// fallback when no pprof-format profile exists for annotation.
type functionSearch struct {
	root  string
	decl  *regexp.Regexp
	found string
	line  int
	files int
}

func FindFunctionInRepo(root, functionName string) (string, int, bool) {
	if functionName == "" {
		return "", 0, false
	}
	s := functionSearch{root: root, decl: regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(functionName) + `\(`)}
	_ = filepath.WalkDir(root, s.visit)
	return s.found, s.line, s.found != ""
}

func (s *functionSearch) visit(path string, d os.DirEntry, err error) error {
	if err != nil || s.found != "" {
		if s.found != "" {
			return filepath.SkipAll
		}
		return nil
	}
	if d.IsDir() {
		return skipIgnoredDir(d.Name())
	}
	return s.matchFile(path)
}

func skipIgnoredDir(base string) error {
	if base == ".git" || base == "vendor" || base == "testdata" {
		return filepath.SkipDir
	}
	return nil
}

func (s *functionSearch) matchFile(path string) error {
	if !strings.HasSuffix(path, ".go") {
		return nil
	}
	s.files++
	if s.files > 2000 {
		return filepath.SkipAll
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	loc := s.decl.FindIndex(data)
	if loc == nil {
		return nil
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		rel = path
	}
	s.found = filepath.ToSlash(rel)
	s.line = 1 + strings.Count(string(data[:loc[0]]), "\n")
	return filepath.SkipAll
}

func atoi(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
