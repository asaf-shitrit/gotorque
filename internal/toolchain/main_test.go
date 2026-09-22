package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain clears the variables git uses to pick a repository before any test
// runs. Fixture helpers such as setupGitRepo run git directly in temporary
// directories, and inside a commit hook git exports GIT_INDEX_FILE, plus
// GIT_DIR when the commit is made in a linked worktree. Inherited, those aimed
// setupGitRepo's init, config and commit at the repository being committed:
// it turned that repository bare, wrote a test identity into its config, and
// committed fixture trees onto its branch.
func TestMain(m *testing.M) {
	for _, name := range gitScopingEnv {
		_ = os.Unsetenv(name)
	}
	m.Run()
}

// envFixtureChild marks the re-executed test binary in
// TestFixtureHelpersIgnoreAnInheritedRepository.
const envFixtureChild = "GOTORQUE_FIXTURE_CHILD"

// TestFixtureHelpersIgnoreAnInheritedRepository re-runs this test binary the
// way a commit hook would, with GIT_DIR and GIT_INDEX_FILE naming a victim
// repository, and has the child build a fixture with setupGitRepo. The victim
// must come out untouched.
func TestFixtureHelpersIgnoreAnInheritedRepository(t *testing.T) {
	if os.Getenv(envFixtureChild) != "" {
		setupGitRepo(t)
		return
	}
	victim := setupGitRepo(t)
	before := repositoryState(t, victim)

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFixtureHelpersIgnoreAnInheritedRepository$") //nolint:gosec // re-executes this test binary, the only way to hand a child the environment a hook would
	cmd.Env = append(os.Environ(),
		envFixtureChild+"=1",
		"GIT_DIR="+filepath.Join(victim, ".git"),
		"GIT_INDEX_FILE="+filepath.Join(victim, ".git", "index"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child test binary: %v\n%s", err, out)
	}
	if after := repositoryState(t, victim); after != before {
		t.Fatalf("a fixture helper wrote into the inherited repository:\nbefore: %s\nafter:  %s", before, after)
	}
}

// repositoryState captures what a leaked fixture helper changes: the config
// (core.bare, user identity) and the number of commits.
func repositoryState(t *testing.T, repo string) string {
	t.Helper()
	config, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-list", "--all", "--count")
	cmd.Dir = repo
	count, err := cmd.Output()
	if err != nil {
		t.Fatalf("count commits: %v", err)
	}
	return strings.ReplaceAll(string(config), "\n", " ") + " commits=" + strings.TrimSpace(string(count))
}
