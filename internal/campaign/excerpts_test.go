package campaign

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

func TestExtractExcerptsCapsUsableExcerptsNotCandidates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hot.go"), []byte("package p\n\nfunc Hot() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Five unparseable locations ahead of a good one previously consumed the
	// whole candidate window and yielded nothing.
	hotPaths := []agents.HotPath{
		{Location: "compiler (line 121)"},
		{Location: "query.go (file-level)"},
		{Location: "testing.(*B).runN"},
		{Location: "parser (line 558)"},
		{Location: "cli input"},
		{Location: "hot.go:3"},
	}
	excerpts, err := extractExcerpts(root, hotPaths, defaultMaxExcerpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts) != 1 {
		t.Fatalf("got %d excerpts, want 1", len(excerpts))
	}
	if excerpts[0].Path != "hot.go" {
		t.Errorf("path = %q, want hot.go", excerpts[0].Path)
	}
}

func TestExcerptCandidatesFallBackToDiscovery(t *testing.T) {
	analyst := []agents.HotPath{{Location: "compiler (line 121)"}}
	got := excerptCandidates(analyst, []string{"cli/encoder.go:260"})
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].Location != "compiler (line 121)" {
		t.Errorf("analyst hot path should come first, got %q", got[0].Location)
	}
	if got[1].Location != "cli/encoder.go:260" {
		t.Errorf("discovery location = %q, want cli/encoder.go:260", got[1].Location)
	}
}

// The optimizer can only attack what it can see, so the window count has to
// cover a measured hot list rather than a handful of frames: discovery reports
// 15-29 hot functions on the campaign targets.
func TestExtractExcerptsCoversAMeasuredHotList(t *testing.T) {
	root := t.TempDir()
	var hotPaths []agents.HotPath
	for i := 0; i < maxExcerpts+3; i++ {
		name := fmt.Sprintf("hot%02d.go", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("package p\n\nfunc Hot() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		hotPaths = append(hotPaths, agents.HotPath{Location: name + ":3"})
	}
	excerpts, err := extractExcerpts(root, hotPaths, defaultMaxExcerpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts) != maxExcerpts {
		t.Fatalf("got %d excerpts, want the %d-window budget", len(excerpts), maxExcerpts)
	}
	if maxExcerpts <= 5 {
		t.Fatalf("window budget %d cannot cover a measured hot list", maxExcerpts)
	}
	total := 0
	for _, e := range excerpts {
		total += len(e.Content)
	}
	if total > defaultMaxExcerpts {
		t.Fatalf("excerpts total %d bytes, over the %d-byte budget", total, defaultMaxExcerpts)
	}
}

// TestFileHeadersCarryTheImports: the optimizer is shown each file's header,
// doc comment and imports included, ahead of the first window of that file,
// once per file, and not when the window already starts at line 1.
func TestFileHeadersCarryTheImports(t *testing.T) {
	root := t.TempDir()
	src := "// Package cli implements the tool.\npackage cli\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n" + strings.Repeat("// filler\n", 60) + "func run() { fmt.Fprintln(os.Stdout, 1) }\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "cli.go"), []byte(src), 0o600))
	windows := []orchestrator.SourceExcerpt{
		{Path: "cli.go", StartLine: 30, Content: "window a", HotPath: "cli.go:69"},
		{Path: "cli.go", StartLine: 40, Content: "window b", HotPath: "cli.go:80"},
		{Path: "top.go", StartLine: 1, Content: "whole file", HotPath: "top.go:3"},
	}
	got := withFileHeaders(root, windows)
	require.Len(t, got, 4)
	require.Equal(t, orchestrator.SourceExcerpt{Path: "cli.go", StartLine: 1, HotPath: "cli.go:69",
		Content: "// Package cli implements the tool.\npackage cli\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)"}, got[0])
	require.Equal(t, windows, got[1:])
}

func TestFileHeaderWithoutImportsEndsAtThePackageClause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	require.NoError(t, os.WriteFile(path, []byte("package a\n\nfunc f() {}\n"), 0o600))
	require.Equal(t, "package a", fileHeader(path))
	require.Empty(t, fileHeader(filepath.Join(t.TempDir(), "missing.go")))
}
