package candidate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemapDiffPathsDroppedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "dedupe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "dedupe", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := []byte("--- a/dedupe/main.go\n+++ b/dedupe/main.go\n@@ -1 +1 @@\n-package main\n+package main // changed\n")
	got, changed := RemapDiffPaths(root, patch)
	if !changed {
		t.Fatal("expected remap")
	}
	s := string(got)
	if !contains(s, "a/cmd/dedupe/main.go") || !contains(s, "b/cmd/dedupe/main.go") {
		t.Fatalf("headers not remapped:\n%s", s)
	}
}

func TestRemapDiffPathsLeavesValidAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := []byte("--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-package main\n+package main // x\n")
	got, changed := RemapDiffPaths(root, patch)
	if changed || string(got) != string(patch) {
		t.Fatalf("valid diff rewritten: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
