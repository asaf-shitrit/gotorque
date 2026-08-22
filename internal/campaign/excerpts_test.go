package campaign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/go-agent-optimizer/internal/agents"
)

func lines(n int) string {
	b := strings.Builder{}
	for i := 1; i <= n; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 0))
		b.WriteString("\n")
	}
	return b.String()
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractExcerptsValidPathWithLine(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cmd/app/main.go", lines(200))
	hp := []agents.HotPath{{Location: "cmd/app/main.go:100"}}
	got, err := extractExcerpts(root, hp, defaultMaxExcerpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 excerpt, got %d", len(got))
	}
	e := got[0]
	if e.Path != "cmd/app/main.go" || e.StartLine != 60 || e.HotPath != "cmd/app/main.go:100" {
		t.Fatalf("unexpected excerpt metadata: %+v", e)
	}
	if got, want := strings.Count(e.Content, "\n")+1, maxExcerptLines; got != want {
		t.Fatalf("content lines = %d, want %d", got, want)
	}
}

func TestExtractExcerptsPathWithoutLine(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", lines(10))
	got, err := extractExcerpts(root, []agents.HotPath{{Location: "main.go"}}, defaultMaxExcerpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartLine != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestExtractExcerptsSkipsEscapingAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ok.go", lines(5))
	hps := []agents.HotPath{
		{Location: "/etc/passwd"},
		{Location: "../secrets.txt"},
		{Location: "missing.go:3"},
		{Location: "ok.go:2"},
	}
	got, err := extractExcerpts(root, hps, defaultMaxExcerpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].HotPath != "ok.go:2" {
		t.Fatalf("expected only ok.go:2, got %+v", got)
	}
}

func TestExtractExcerptsTotalSizeCapDropsLaterHotPaths(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("a", 900) + "\n"
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		var b strings.Builder
		for j := 0; j < 20; j++ {
			b.WriteString(big)
		} // ~18KB per file, truncated to the 8KB per-excerpt cap
		writeFile(t, root, name, b.String())
	}
	hps := []agents.HotPath{
		{Location: "a.go"}, {Location: "b.go"}, {Location: "c.go"},
	}
	// 20KB total allows two 8KB excerpts; the third (lowest priority) drops.
	got, err := extractExcerpts(root, hps, 20*1024)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range got {
		if len(e.Content) > maxExcerptBytes {
			t.Fatalf("excerpt exceeds per-excerpt cap: %d", len(e.Content))
		}
		total += len(e.Content)
	}
	if total > 20*1024 {
		t.Fatalf("total %d exceeds cap", total)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 excerpts after capping, got %d", len(got))
	}
	// Later hot paths must be dropped first.
	last := got[len(got)-1]
	if last.HotPath == "c.go" {
		t.Fatalf("lowest-priority hot path c.go should have been dropped")
	}
}
