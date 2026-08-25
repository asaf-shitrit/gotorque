package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/gotorque/internal/mcpserver"
	"example.com/gotorque/internal/toolchain"
	"github.com/stretchr/testify/require"
)

// fakeExecutor stands in for the operating system so InspectRepository can be
// tested against canned `go list` output without invoking the real toolchain.
type fakeExecutor struct {
	stdout []byte
	err    error
	calls  int
}

func (f *fakeExecutor) Run(_ context.Context, in toolchain.Invocation) (toolchain.Result, error) {
	f.calls++
	if f.err != nil {
		return toolchain.Result{Stderr: []byte("boom")}, f.err
	}
	return toolchain.Result{Stdout: f.stdout}, nil
}

const cannedGoList = `
{"ImportPath":"example.com/demo/internal/core","Name":"core"}
{"ImportPath":"example.com/demo/cmd/tool","Name":"main"}
{"ImportPath":"example.com/demo/cmd/other","Name":"main"}
`

func newInspectTestBackend(t *testing.T, executor *fakeExecutor) *engineBackend {
	t.Helper()
	backend := newEngineBackend(t.TempDir())
	backend.inventory = toolchain.New(toolchain.Options{Executor: executor})
	return backend
}

func TestInspectRepositoryInventoriesPackagesAndCommands(t *testing.T) {
	repo := t.TempDir()
	executor := &fakeExecutor{stdout: []byte(cannedGoList)}
	backend := newInspectTestBackend(t, executor)

	result, err := backend.InspectRepository(context.Background(), mcpserver.RepositoryInspectionInput{
		Repository:  repo,
		BuildTarget: "./cmd/tool",
	})
	require.NoError(t, err)
	require.Equal(t, 1, executor.calls)
	require.True(t, strings.HasPrefix(result.ResultURI, "repo://"), "unexpected result URI %q", result.ResultURI)
	require.True(t, strings.HasSuffix(result.ResultURI, "/inventory"), "unexpected result URI %q", result.ResultURI)
	require.Contains(t, result.Summary, "3 packages")
	require.Contains(t, result.Summary, "2 command entry points")

	document, err := backend.ReadResource(context.Background(), result.ResultURI)
	require.NoError(t, err)
	require.Equal(t, "application/json", document.MIMEType)

	var inventory repositoryInventory
	require.NoError(t, json.Unmarshal([]byte(document.Text), &inventory))
	require.Equal(t, "repo://"+inventory.Hash+"/inventory", result.ResultURI)
	require.Equal(t, []string{"example.com/demo/cmd/other", "example.com/demo/cmd/tool", "example.com/demo/internal/core"}, inventory.Packages)
	require.Equal(t, []string{"example.com/demo/cmd/other", "example.com/demo/cmd/tool"}, inventory.Commands)

	// The inventory document must also be persisted below the harness root.
	_, err = os.Stat(filepath.Join(backend.root, "inspection", inventory.Hash, "inventory.json"))
	require.NoError(t, err)
}

func TestInspectRepositoryRejectsRelativeOrMissingRepository(t *testing.T) {
	executor := &fakeExecutor{stdout: []byte(cannedGoList)}
	backend := newInspectTestBackend(t, executor)

	_, err := backend.InspectRepository(context.Background(), mcpserver.RepositoryInspectionInput{Repository: "relative/path"})
	require.ErrorContains(t, err, "must be an absolute path")

	_, err = backend.InspectRepository(context.Background(), mcpserver.RepositoryInspectionInput{Repository: filepath.Join(t.TempDir(), "missing")})
	require.Error(t, err)
	require.ErrorContains(t, err, "missing")

	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = backend.InspectRepository(context.Background(), mcpserver.RepositoryInspectionInput{Repository: file})
	require.ErrorContains(t, err, "not a directory")

	require.Zero(t, executor.calls, "go list must not run for invalid inputs")
}

func TestInspectRepositoryPropagatesGoListFailure(t *testing.T) {
	executor := &fakeExecutor{err: errors.New("exit status 1")}
	backend := newInspectTestBackend(t, executor)

	_, err := backend.InspectRepository(context.Background(), mcpserver.RepositoryInspectionInput{Repository: t.TempDir()})
	require.Error(t, err)
	require.ErrorContains(t, err, "inventory Go packages")
	require.ErrorContains(t, err, "go list")
}

func TestReadRepositoryResourceUnknownInspection(t *testing.T) {
	backend := newEngineBackend(t.TempDir())
	_, err := backend.ReadResource(context.Background(), "repo://deadbeef/inventory")
	require.ErrorContains(t, err, "deadbeef")

	_, err = backend.ReadResource(context.Background(), "repo://deadbeef/coverage")
	require.ErrorContains(t, err, "unknown repository resource")
}
