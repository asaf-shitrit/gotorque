package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocumentedMinimalShapeLoads keeps docs/target-manifest.md honest. Its
// "Minimal shape" block is the first thing anyone writing a target copies, and
// it silently stopped loading once performance and campaign became required:
// the prose still called them optional, so the example failed schema
// validation with "missing properties" for anyone who followed it.
func TestDocumentedMinimalShapeLoads(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "target-manifest.md")
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	block, ok := firstJSONBlock(string(data))
	if !ok {
		t.Fatalf("%s has no ```json block to check", docPath)
	}

	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
		t.Fatalf("write example: %v", err)
	}
	m, err := LoadFile(path)
	if err != nil {
		t.Fatalf("documented minimal shape does not load: %v", err)
	}

	// The doc promises the loader supplies these when the objects are empty.
	if m.Performance.PrimaryMetric != DefaultPrimaryMetric {
		t.Errorf("primary metric = %q, want %q", m.Performance.PrimaryMetric, DefaultPrimaryMetric)
	}
	if m.Campaign.MaxCandidatePatches != DefaultMaxCandidatePatches {
		t.Errorf("max candidate patches = %d, want %d", m.Campaign.MaxCandidatePatches, DefaultMaxCandidatePatches)
	}
}

// firstJSONBlock returns the contents of the first fenced ```json block.
func firstJSONBlock(doc string) (string, bool) {
	const fence = "```json\n"
	start := strings.Index(doc, fence)
	if start < 0 {
		return "", false
	}
	rest := doc[start+len(fence):]
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// TestCheckedInTargetManifestsLoad guards the manifests CI validates, so a
// schema change that invalidates a shipped target fails here rather than in a
// campaign.
func TestCheckedInTargetManifestsLoad(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "targets", "*", "manifest.json"))
	if err != nil {
		t.Fatalf("glob targets: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no checked-in target manifests found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			if _, err := LoadFile(path); err != nil {
				t.Errorf("%s does not load: %v", path, err)
			}
		})
	}
}
