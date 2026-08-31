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

func TestHotLocationLocation(t *testing.T) {
	cases := []struct {
		name string
		loc  HotLocation
		want string
	}{
		{"with line", HotLocation{Path: "a/b.go", Line: 42}, "a/b.go:42"},
		{"zero line", HotLocation{Path: "a/b.go", Line: 0}, "a/b.go"},
		{"negative line", HotLocation{Path: "a/b.go", Line: -1}, "a/b.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.loc.Location(); got != tc.want {
				t.Fatalf("Location() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestItoa(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{180, "180"},
		{20260831, "20260831"},
	}
	for _, tc := range cases {
		if got := itoa(tc.n); got != tc.want {
			t.Fatalf("itoa(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFindFunctionInRepoEmptyName(t *testing.T) {
	if _, _, ok := FindFunctionInRepo(t.TempDir(), ""); ok {
		t.Fatal("expected not-ok for empty function name")
	}
}

func TestFindFunctionInRepoIgnoresUnreadableAndNonGoFiles(t *testing.T) {
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
	write("readme.txt", "func Target() {}\n")
	write(".git/HEAD", "func Target() {}\n")
	write("testdata/fixture.go", "package fixture\n\nfunc Target() {}\n")
	write("real/target.go", "package real\n\nfunc Target() {\n\treturn\n}\n")

	path, line, ok := FindFunctionInRepo(root, "Target")
	if !ok {
		t.Fatal("expected to find Target")
	}
	if path != filepath.ToSlash(filepath.Join("real", "target.go")) {
		t.Fatalf("path = %q, want real/target.go", path)
	}
	if line != 3 {
		t.Fatalf("line = %d, want 3", line)
	}
}

func TestColonLineNumberRejectsNonNumericPrefix(t *testing.T) {
	if n := colonLineNumber("abc:not code"); n != 0 {
		t.Fatalf("colonLineNumber = %d, want 0", n)
	}
	if n := colonLineNumber("no colon here"); n != 0 {
		t.Fatalf("colonLineNumber = %d, want 0", n)
	}
	if n := colonLineNumber("42:code"); n != 42 {
		t.Fatalf("colonLineNumber = %d, want 42", n)
	}
}

func TestAtoiRejectsNonNumeric(t *testing.T) {
	if n := atoi("12a"); n != 0 {
		t.Fatalf("atoi = %d, want 0", n)
	}
	if n := atoi("99"); n != 99 {
		t.Fatalf("atoi = %d, want 99", n)
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
