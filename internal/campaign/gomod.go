package campaign

import (
	"os"
	"path/filepath"
	"strings"
)

// goDirective reads the go directive of the module that builds the target:
// the go.mod in the manifest's build directory, or the repository's own.
func goDirective(repo, buildDir string) string {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(buildDir), "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
