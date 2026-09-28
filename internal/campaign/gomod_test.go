package campaign

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoDirective(t *testing.T) {
	repo := t.TempDir()
	require.Empty(t, goDirective(repo, ""))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/m\n\ngo 1.20\n\nrequire x v1\n"), 0o600))
	require.Equal(t, "1.20", goDirective(repo, ""))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cmd", "tool"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cmd", "tool", "go.mod"), []byte("module example.com/m/cmd/tool\ngo 1.22.1\n"), 0o600))
	require.Equal(t, "1.22.1", goDirective(repo, "cmd/tool"))
}
