// Package candidate validates model-proposed source patches before Git sees
// them. It does not apply patches or execute commands.
package candidate

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const devNull = "/dev/null"

// dependencyFiles fix the build the baseline was measured against. go.work and
// go.work.sum belong here with go.mod: a workspace file can substitute a local
// copy for any module without go.mod changing at all.
var dependencyFiles = map[string]bool{"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true, "default.pgo": true}

type Policy struct {
	MaxBytes             int
	ProhibitedTechniques []string
}

type Result struct{ Files []string }

// ValidateUnifiedDiff rejects a patch that names a path a candidate may not
// touch. Every name the diff carries is checked, not only the `+++` target:
// git apply and GNU patch each choose which header to believe, and GNU patch,
// given `--- a/go.mod` and `+++ b/other.go`, edits go.mod. Validation is a
// fast first answer for the optimizer; the authoritative check is the one
// Prepare runs against the applied worktree.
func ValidateUnifiedDiff(patch string, policy Policy) (Result, error) {
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = 1 << 20
	}
	if err := rejectPatchPreamble(patch, policy); err != nil {
		return Result{}, err
	}
	files, err := collectPatchFiles(patch)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 || !strings.Contains(patch, "@@") {
		return Result{}, errors.New("malformed unified diff")
	}
	slices.Sort(files)
	return Result{Files: files}, nil
}

func rejectPatchPreamble(patch string, policy Policy) error {
	if len(patch) == 0 {
		return errors.New("patch is empty")
	}
	if len(patch) > policy.MaxBytes {
		return fmt.Errorf("patch exceeds %d byte limit", policy.MaxBytes)
	}
	if strings.ContainsRune(patch, '\x00') {
		return errors.New("binary patches are not supported")
	}
	for _, technique := range policy.ProhibitedTechniques {
		if technique != "" && strings.Contains(strings.ToLower(patch), strings.ToLower(technique)) {
			return fmt.Errorf("patch contains prohibited technique %q", technique)
		}
	}
	return nil
}

// collectPatchFiles checks every header line and returns the paths the diff's
// `---` and `+++` lines name, with their a/ or b/ prefix removed.
func collectPatchFiles(patch string) ([]string, error) {
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		// GNU patch strips the carriage returns of a CRLF diff, so a header is
		// read the same way here.
		line = strings.TrimSuffix(line, "\r")
		if err := rejectHeaderLine(line); err != nil {
			return nil, err
		}
		names, err := linePaths(line)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			seen[name] = true
		}
	}
	files := make([]string, 0, len(seen))
	for name := range seen {
		files = append(files, name)
	}
	return files, nil
}

// rejectHeaderLine refuses the Git extended headers that change what a path
// is rather than what it holds. A symlink can point a harmless name at a
// protected file or out of the worktree, a gitlink swaps in another
// repository, and a mode change is never part of a performance fix.
func rejectHeaderLine(line string) error {
	switch {
	case strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode "):
		return fmt.Errorf("file mode changes are not allowed: %q", line)
	case strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode "):
		return requireRegularMode(line, line[strings.LastIndexByte(line, ' ')+1:])
	case strings.HasPrefix(line, "index "):
		// `index <old>..<new> <mode>` carries the mode of a file edited in
		// place, and 120000 there is an edit to a symlink's target.
		if fields := strings.Fields(line); len(fields) == 3 && strings.Contains(fields[1], "..") {
			return requireRegularMode(line, fields[2])
		}
	case strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files "):
		return errors.New("binary patches are not supported")
	}
	return nil
}

func requireRegularMode(line, mode string) error {
	if mode == "100644" || mode == "100755" {
		return nil
	}
	return fmt.Errorf("only regular files may be patched; symlinks, submodules, and mode %s are not allowed: %q", mode, line)
}

// namingLines are the header lines git apply or GNU patch may take a file name
// from. listed marks the unified-diff headers whose name is reported in
// Result.Files; the rest are only checked. Rename and copy names carry no a/
// or b/ prefix, `*** ` names a file in a context diff, and GNU patch falls
// back to an `Index: ` name when the others are missing.
var namingLines = []struct {
	prefix string
	listed bool
}{
	{"--- ", true}, {"+++ ", true},
	{"diff --git ", false},
	{"rename from ", false}, {"rename to ", false},
	{"copy from ", false}, {"copy to ", false},
	{"*** ", false}, {"Index: ", false},
}

func linePaths(line string) ([]string, error) {
	for _, header := range namingLines {
		if text, ok := strings.CutPrefix(line, header.prefix); ok {
			return headerNames(text, header.listed)
		}
	}
	return nil, nil
}

func headerNames(text string, listed bool) ([]string, error) {
	names, err := splitNames(text)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("malformed patch path")
	}
	for _, name := range names {
		if err := checkNamedPath(name); err != nil {
			return nil, err
		}
	}
	if !listed || names[0] == devNull {
		return nil, nil
	}
	return []string{stripComponent(names[0])}, nil
}

// checkNamedPath checks a header name both as written and as `-p1` reads it.
// A name without an a/ or b/ prefix still loses its first component to -p1
// (`z/../go.mod` becomes `../go.mod`), while rename and copy names are used
// as written, so a name has to pass in both readings.
func checkNamedPath(name string) error {
	if name == devNull {
		return nil
	}
	if err := rejectPatchPath(stripComponent(name)); err != nil {
		return err
	}
	return rejectPatchPath(path.Clean(name))
}

// stripComponent applies `-p1`: it drops everything through the first slash.
// A name without a slash has nothing to strip and is kept as written.
func stripComponent(name string) string {
	if _, rest, ok := strings.Cut(name, "/"); ok {
		name = strings.TrimLeft(rest, "/")
	}
	return path.Clean(name)
}

// splitNames splits the text after a header prefix into file names. A quoted
// name is unquoted, because git and GNU patch quote a name holding a space,
// tab, or non-ASCII byte and read the unquoted form; `"b/test\144ata/x"` is
// testdata/x to both. Unquoted text is split on whitespace, which yields
// every name a tool could take plus trailing timestamps, and so only ever
// errs toward checking more.
func splitNames(text string) ([]string, error) {
	var names []string
	for {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		if text == "" {
			return names, nil
		}
		name, rest, err := nextName(text)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		text = rest
	}
}

func nextName(text string) (name, rest string, err error) {
	if text[0] != '"' {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 {
			return text, "", nil
		}
		return text[:end], text[end:], nil
	}
	end := closingQuote(text)
	if end < 0 {
		return "", "", fmt.Errorf("unterminated quoted patch path %q", text)
	}
	name, err = strconv.Unquote(text[:end+1])
	if err != nil {
		return "", "", fmt.Errorf("malformed quoted patch path %q", text[:end+1])
	}
	return name, text[end+1:], nil
}

func closingQuote(text string) int {
	for i := 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// rejectPatchPath refuses a cleaned, slash-separated, repository-relative path
// a candidate may not create, edit, rename, or delete.
func rejectPatchPath(name string) error {
	if pathEscapesRepo(name) {
		return fmt.Errorf("patch path %q escapes repository", name)
	}
	if err := rejectProtectedPath(name); err != nil {
		return err
	}
	if strings.Contains(name, "gotorque_pprof") || strings.Contains(name, "gotorque_trace") {
		return fmt.Errorf("diagnostic code %q may not enter candidate diff", name)
	}
	return nil
}

// rejectProtectedPath refuses the files that judge a candidate rather than
// make up the program. A patch that edits a test, a golden file under
// testdata/, or the module graph can make the gate pass instead of passing
// it, and the optimizer is shown which tests failed, so rewriting the
// assertion is exactly the shortcut it would otherwise be steered toward.
func rejectProtectedPath(name string) error {
	segments := strings.Split(name, "/")
	base := segments[len(segments)-1]
	switch {
	case strings.HasSuffix(base, "_test.go") || slices.Contains(segments, "testdata"):
		return fmt.Errorf("test file %q is off-limits: *_test.go files and testdata/ define the test gate that judges the patch", name)
	case dependencyFiles[base] || slices.Contains(segments, "vendor"):
		return fmt.Errorf("dependency or PGO file %q is off-limits: go.mod, go.sum, go.work, default.pgo, and vendor/ fix the build the baseline was measured against", name)
	case slices.Contains(segments, ".git") || base == ".gitattributes":
		// .gitattributes decides how Git compares worktree files, and Git's
		// view of the applied worktree is what the post-apply check reads.
		return fmt.Errorf("git metadata %q is off-limits", name)
	}
	return nil
}

func pathEscapesRepo(name string) bool {
	return name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../")
}
