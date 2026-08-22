package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"example.com/go-agent-optimizer/internal/version"
	"github.com/stretchr/testify/require"
)

func TestVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"version", "--json"})

	require.NoError(t, cmd.Execute())
	var got version.Info
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	require.Equal(t, version.Current(), got)
	require.Empty(t, stderr.String())
}

func TestUnknownCommandDoesNotPrintUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"missing"})

	require.Error(t, cmd.Execute())
	require.Empty(t, stdout.String())
	require.Empty(t, stderr.String())
}

func TestManifestValidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := New(Dependencies{Stdout: &stdout, Stderr: &stderr})
	cmd.SetArgs([]string{"manifest", "validate", filepath.Join("..", "..", "targets", "gojq", "manifest.json")})

	require.NoError(t, cmd.Execute())
	require.Contains(t, stdout.String(), "valid target manifest")
	require.Empty(t, stderr.String())
}
