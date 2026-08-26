package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePprofList(t *testing.T) {
	output := `ROUTINE ======================== github.com/itchyny/gojq/cli.(*cli).run in /home/u/gojq/cli/cli.go
     10     15 (line 180) func (cli *cli) run() error
      .      .   181:	if err := cli.parseArgs(); err != nil {
      .      .   182:		return err
`
	path, line, ok := ParsePprofList(output)
	if !ok {
		t.Fatal("expected ok")
	}
	wantPath := "/home/u/gojq/cli/cli.go"
	gotPath := filepath.ToSlash(path)
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
	if line != 180 {
		t.Fatalf("line = %d, want 180", line)
	}
}

func TestParsePprofListRejectsGarbage(t *testing.T) {
	if _, _, ok := ParsePprofList("File: binary\nType: cpu\n"); ok {
		t.Fatal("expected not-ok for output without a routine header")
	}
}

func TestFindFunctionInRepo(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("internal/scan/scan.go", "package scan\n\ntype scanner struct{}\n\nfunc (s *scanner) Scan() int {\n\treturn 1\n}\n")
	write("vendor/v/v.go", "package v\n\nfunc Stolen() {}\n")

	path, line, ok := FindFunctionInRepo(root, "Scan")
	if !ok {
		t.Fatal("method declaration not found")
	}
	if path != filepath.ToSlash(filepath.Join("internal", "scan", "scan.go")) || line != 5 {
		t.Fatalf("path=%q line=%d", path, line)
	}

	path, line, ok = FindFunctionInRepo(root, "main")
	if !ok || line != 3 {
		t.Fatalf("main: path=%q line=%d ok=%v", path, line, ok)
	}

	if _, _, ok := FindFunctionInRepo(root, "Stolen"); ok {
		t.Fatal("vendored code must be skipped")
	}
	if _, _, ok := FindFunctionInRepo(root, "NoSuchFunction"); ok {
		t.Fatal("missing function must report not-found")
	}
}
