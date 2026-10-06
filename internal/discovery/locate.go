package discovery

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// symlinkForms returns the path as given and, when it differs, its
// symlink-resolved form.
func symlinkForms(path string) []string {
	forms := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		forms = append(forms, resolved)
	}
	return forms
}

// EnclosingFunction strips closure suffixes until the declared function
// remains. Profile frames name closures in forms no `func` declaration uses:
// pkg.outer.func1 for a closure, pkg.outer.func1.2 when nested.
func EnclosingFunction(name string) string {
	for {
		trimmed, ok := trimClosureSuffix(name)
		if !ok {
			return name
		}
		name = trimmed
	}
}

var closureSuffix = regexp.MustCompile(`\.func\d+$`)

func trimClosureSuffix(name string) (string, bool) {
	if loc := closureSuffix.FindStringIndex(name); loc != nil {
		return name[:loc[0]], true
	}
	// Nested closures append an ordinal: outer.func1.2.
	if idx := strings.LastIndex(name, "."); idx > 0 {
		if _, err := strconv.Atoi(name[idx+1:]); err == nil {
			return name[:idx], true
		}
	}
	return name, false
}

// RepoRelative rewrites a profiler's absolute source path into the
// repository-relative form the excerpt collector requires, and rejects paths
// outside the repository.
//
// `go tool pprof -list` reports absolute paths. extractExcerpts refuses those
// because an absolute location is indistinguishable from one escaping the
// repository, so every profiled frame resolved to a location no source window
// could ever be read from: a target checked out under a path the profiler
// echoed back produced one usable excerpt out of eleven measured functions.
// Frames in the standard library or module cache are dropped outright rather
// than kept as bare paths, since no patch this campaign may write can reach
// them and they otherwise occupy the excerpt budget.
func RepoRelative(repository, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path), true
	}
	// Both sides are compared in raw and symlink-resolved form. macOS resolves
	// a temporary root through /private while a source file the profiler named
	// may not resolve at all, and comparing one resolved path against one raw
	// path reports a file inside the repository as escaping it.
	for _, root := range symlinkForms(repository) {
		for _, candidate := range symlinkForms(path) {
			rel, err := filepath.Rel(root, candidate)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}
