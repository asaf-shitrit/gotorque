package candidate

import (
	"os"
	"path/filepath"
	"strings"
)

// RemapDiffPaths rewrites diff file headers whose target does not exist in
// worktree to the unique repository file sharing the same path suffix.
// Models frequently drop leading directories (cmd/, internal/) from paths;
// when the suffix identifies exactly one file the diff is corrected instead
// of rejected. Returns whether any header changed.
func RemapDiffPaths(worktree string, patch []byte) ([]byte, bool) {
	text := string(patch)
	lines := strings.Split(text, "\n")
	changed := false
	for i, line := range lines {
		prefix, namespace, target, extra, ok := parseRemapHeader(line)
		if !ok {
			continue
		}
		full := filepath.Join(worktree, target)
		if _, err := os.Stat(full); err == nil {
			continue // exists; leave untouched
		}
		match, ok := uniqueSuffixMatch(worktree, target)
		if !ok {
			continue
		}
		lines[i] = prefix + namespace + match + extra
		changed = true
	}
	if !changed {
		return patch, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

func parseRemapHeader(line string) (prefix, namespace, target, extra string, ok bool) {
	var rawPath string
	if strings.HasPrefix(line, "--- ") {
		prefix, namespace = "--- ", "a/"
		rawPath = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
	} else if strings.HasPrefix(line, "+++ ") {
		prefix, namespace = "+++ ", "b/"
		rawPath = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
	} else {
		return "", "", "", "", false
	}
	if rawPath == "/dev/null" {
		return "", "", "", "", false
	}
	clean := strings.Fields(rawPath)
	if len(clean) == 0 {
		return "", "", "", "", false
	}
	target = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(clean[0], "b/")))
	if target == "." || filepath.IsAbs(target) || strings.HasPrefix(target, "../") {
		return "", "", "", "", false
	}
	return prefix, namespace, target, restoreFields(clean), true
}

func restoreFields(fields []string) string {
	if len(fields) <= 1 {
		return ""
	}
	return " " + strings.Join(fields[1:], " ")
}

// uniqueSuffixMatch finds the single repository file whose slash path ends
// with "/"+target (or equals target).
func uniqueSuffixMatch(root, target string) (string, bool) {
	var match string
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		return matchSuffixFile(root, target, path, d, err, &match, &count)
	})
	if count == 1 {
		return match, true
	}
	return "", false
}

func matchSuffixFile(root, target, path string, d os.DirEntry, err error, match *string, count *int) error {
	if *count > 1 {
		return filepath.SkipAll
	}
	if err != nil {
		return nil
	}
	if skip, dirErr := skipWalkDir(d); skip {
		return dirErr
	}
	rel, ok := relSuffixMatch(root, target, path)
	if !ok {
		return nil
	}
	*count++
	if *count == 1 {
		*match = rel
	}
	if *count > 1 {
		return filepath.SkipAll
	}
	return nil
}

func skipWalkDir(d os.DirEntry) (bool, error) {
	if d == nil || !d.IsDir() {
		return false, nil
	}
	if d.Name() == ".git" || d.Name() == "vendor" {
		return true, filepath.SkipDir
	}
	return true, nil
}

func relSuffixMatch(root, target, path string) (string, bool) {
	if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(target, ".go") && !strings.HasSuffix(path, target) {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == target || strings.HasSuffix("/"+rel, "/"+target) {
		return rel, true
	}
	return "", false
}
