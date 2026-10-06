package discovery

import (
	"path/filepath"
	"testing"
)

func TestEnclosingFunctionUnwrapsClosures(t *testing.T) {
	tests := map[string]string{
		"github.com/itchyny/gojq/cli.newJSONInputIter.func1": "github.com/itchyny/gojq/cli.newJSONInputIter",
		"pkg.outer.func1.2":                      "pkg.outer",
		"github.com/x/y.(*Encoder).encode":       "github.com/x/y.(*Encoder).encode",
		"github.com/x/y.(*Encoder).encode.func3": "github.com/x/y.(*Encoder).encode",
		"main.main":                              "main.main",
	}
	for name, want := range tests {
		if got := EnclosingFunction(name); got != want {
			t.Errorf("EnclosingFunction(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRepoRelativeRewritesProfilerPaths(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"absolute inside repo", filepath.Join(root, "statements.go"), "statements.go", true},
		{"absolute nested", filepath.Join(root, "cmd", "main.go"), "cmd/main.go", true},
		{"already relative", "identifier.go", "identifier.go", true},
		{"standard library", "/opt/homebrew/Cellar/go/1.27.0/libexec/src/unicode/graphic.go", "", false},
		{"repository root itself", root, "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RepoRelative(root, tt.path)
			if ok != tt.wantOK {
				t.Fatalf("RepoRelative(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("RepoRelative(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
