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

// TestParsePprofListReadsRealRows parses real `go tool pprof -list` output
// (chroma's (*LexerRegistry).Get), whose rows open with the flat and cum
// columns: the line number is the third field, never the first.
func TestParsePprofListReadsRealRows(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pprof-list-method.txt"))
	if err != nil {
		t.Fatal(err)
	}
	path, line, ok := ParsePprofList(string(data))
	if !ok || filepath.ToSlash(path) != "/repo/chroma/registry.go" || line != 73 {
		t.Fatalf("got %q:%d ok=%v, want /repo/chroma/registry.go:73", path, line, ok)
	}
}
