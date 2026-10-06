package discovery

import (
	"os"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// TestMain clears the variables git uses to pick a repository before any test
// runs: inside a commit hook git exports GIT_INDEX_FILE, plus GIT_DIR in a
// linked worktree, and a test that runs go or git in a temporary directory
// would inherit them and point at the repository being committed. See the
// toolchain package's TestMain for what that did.
func TestMain(m *testing.M) {
	for _, name := range toolchain.GitScopingEnv() {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
