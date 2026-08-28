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

func collectPatchFiles(patch string) ([]string, error) {
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		name, skip, err := plusPlusPath(line)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if err := rejectPatchPath(name); err != nil {
			return nil, err
		}
		seen[name] = true
	}
	files := make([]string, 0, len(seen))
	for name := range seen {
		files = append(files, name)
	}
	return files, nil
}

func plusPlusPath(line string) (name string, skip bool, err error) {
	if !strings.HasPrefix(line, "+++ ") {
		return "", true, nil
	}
	name = strings.TrimPrefix(line, "+++ ")
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return "", false, errors.New("malformed patch path")
	}
	name = fields[0]
	if name == "/dev/null" {
		return "", true, nil
	}
	name = strings.TrimPrefix(name, "b/")
	return filepath.ToSlash(filepath.Clean(name)), false, nil
}

func rejectPatchPath(name string) error {
	if pathEscapesRepo(name) {
		return fmt.Errorf("patch path %q escapes repository", name)
	}
	base := filepath.Base(name)
	if prohibitedFiles[base] || strings.HasPrefix(name, "vendor/") {
		return fmt.Errorf("dependency or PGO file %q may not be changed", name)
	}
	if strings.Contains(name, "gotorque_pprof") || strings.Contains(name, "gotorque_trace") {
		return fmt.Errorf("diagnostic code %q may not enter candidate diff", name)
	}
	return nil
}

func pathEscapesRepo(name string) bool {
	return name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../")
}
