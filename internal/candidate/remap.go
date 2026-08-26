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
		var prefix, namespace, rawPath string
		switch {
		case strings.HasPrefix(line, "--- "):
			prefix, namespace = "--- ", "a/"
			rawPath = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			prefix, namespace = "+++ ", "b/"
			rawPath = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
		default:
			continue
		}
		if rawPath == "/dev/null" {
			continue
		}
		clean := strings.Fields(rawPath)
		if len(clean) == 0 {
			continue
		}
		target := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(clean[0], "b/")))
		if target == "." || filepath.IsAbs(target) || strings.HasPrefix(target, "../") {
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
		lines[i] = prefix + namespace + match + restoreFields(clean)
		changed = true
	}
	if !changed {
		return patch, false
	}
	return []byte(strings.Join(lines, "\n")), true
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
	suffix := "/" + target
	var match string
	count := 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || match != "" && count > 1 {
			if count > 1 {
				return filepath.SkipAll
			}
			return nil
		}
		if d != nil && d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(target, ".go") &&
			!strings.HasSuffix(path, target) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == target || strings.HasSuffix("/"+rel, suffix) {
			count++
			if count == 1 {
				match = rel
			}
			if count > 1 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if count == 1 {
		return match, true
	}
	return "", false
}
