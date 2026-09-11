package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"example.com/gotorque/internal/version"
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

// A resumed campaign reads its manifest from persisted state, and --resume
// rejects an explicit --manifest, so requiring one would make resuming a
// model-driven campaign impossible to express.
func TestResumeDoesNotRequireManifest(t *testing.T) {
	_, _, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{resume: "/tmp/campaign", runADK: true})
	require.NoError(t, err)
}

func TestFreshADKRunStillRequiresManifest(t *testing.T) {
	_, _, err := configureOptimizeAgents(context.Background(), io.Discard, optimizeFlags{runADK: true})
	require.ErrorContains(t, err, "--manifest is required")
}
