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
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "ROUTINE ") {
			break
		}
		lineNumber := 0
		if m := pprofLineMarker.FindStringSubmatch(trimmed); m != nil {
			lineNumber = atoi(m[1])
		} else if colon := strings.Index(trimmed, ":"); colon > 0 {
			digits := true
			for _, ch := range trimmed[:colon] {
				if ch < '0' || ch > '9' {
					digits = false
					break
				}
				lineNumber = lineNumber*10 + int(ch-'0')
			}
			if !digits {
				lineNumber = 0
			}
		}
		// The function starts at its lowest sampled source line.
		if lineNumber > 0 && (line == 0 || lineNumber < line) {
			line = lineNumber
		}
	}
	if path == "" {
		return "", 0, false
	}
	return path, line, true
}

// FindFunctionInRepo locates a function declaration by name under root,
// searching .go files breadth-first with a bounded file count. It is the
// fallback when no pprof-format profile exists for annotation.
func FindFunctionInRepo(root, functionName string) (string, int, bool) {
	if functionName == "" {
		return "", 0, false
	}
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(functionName) + `\(`)
	found := ""
	line := 0
	files := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			if found != "" {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		if files > 2000 {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		loc := decl.FindIndex(data)
		if loc == nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		found = filepath.ToSlash(rel)
		line = 1 + strings.Count(string(data[:loc[0]]), "\n")
		return filepath.SkipAll
	})
	return found, line, found != ""
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
