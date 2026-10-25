package cli

import (
	"os"
	"testing"

	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// TestMain drops the variables that redirect git away from the directory it
// runs in: the verify fixture builds a Git repository with git directly, and
// under a commit hook those variables would point it at the repository being
// committed instead.
func TestMain(m *testing.M) {
	for _, key := range toolchain.GitScopingEnv() {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
