package campaign

import (
	"os"
	"testing"

	"example.com/gotorque/internal/toolchain"
)

// TestMain clears the variables git uses to pick a repository before any test
// runs. The git helper here runs git directly in temporary directories, and
// inside a commit hook git exports GIT_INDEX_FILE, plus GIT_DIR in a linked
// worktree; inherited, they would point it at the repository being committed.
// See the toolchain package's TestMain for what that did.
func TestMain(m *testing.M) {
	for _, name := range toolchain.GitScopingEnv() {
		_ = os.Unsetenv(name)
	}
	m.Run()
}
