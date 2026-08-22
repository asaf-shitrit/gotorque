// Package candidate validates model-proposed source patches before Git sees
// them. It does not apply patches or execute commands.
package candidate

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

var prohibitedFiles = map[string]bool{"go.mod": true, "go.sum": true, "default.pgo": true}

type Policy struct {
	MaxBytes             int
	ProhibitedTechniques []string
}

type Result struct{ Files []string }

func ValidateUnifiedDiff(patch string, policy Policy) (Result, error) {
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = 1 << 20
	}
	if len(patch) == 0 {
		return Result{}, errors.New("patch is empty")
	}
	if len(patch) > policy.MaxBytes {
		return Result{}, fmt.Errorf("patch exceeds %d byte limit", policy.MaxBytes)
	}
	if strings.ContainsRune(patch, '\x00') {
		return Result{}, errors.New("binary patches are not supported")
	}
	for _, technique := range policy.ProhibitedTechniques {
		if technique != "" && strings.Contains(strings.ToLower(patch), strings.ToLower(technique)) {
			return Result{}, fmt.Errorf("patch contains prohibited technique %q", technique)
		}
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		name := strings.TrimPrefix(line, "+++ ")
		fields := strings.Fields(name)
		if len(fields) == 0 {
			return Result{}, errors.New("malformed patch path")
		}
		name = fields[0]
		if name == "/dev/null" {
			continue
		}
		name = strings.TrimPrefix(name, "b/")
		name = filepath.ToSlash(filepath.Clean(name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return Result{}, fmt.Errorf("patch path %q escapes repository", name)
		}
		base := filepath.Base(name)
		if prohibitedFiles[base] || strings.HasPrefix(name, "vendor/") {
			return Result{}, fmt.Errorf("dependency or PGO file %q may not be changed", name)
		}
		if strings.Contains(name, "goharness_pprof") || strings.Contains(name, "goharness_trace") {
			return Result{}, fmt.Errorf("diagnostic code %q may not enter candidate diff", name)
		}
		seen[name] = true
	}
	if len(seen) == 0 || !strings.Contains(patch, "@@") {
		return Result{}, errors.New("malformed unified diff")
	}
	files := make([]string, 0, len(seen))
	for name := range seen {
		files = append(files, name)
	}
	slices.Sort(files)
	return Result{Files: files}, nil
}
