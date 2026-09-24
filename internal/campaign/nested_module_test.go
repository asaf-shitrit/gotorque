package campaign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeNestedModuleRepository builds a fixture repository whose CLI lives in
// its own Go module, cmd/tool, with a `replace ../../` back to the root
// module — the shape ADR 0023 exists for (alecthomas/chroma's cmd/chroma is a
// real example). The root module builds a package the nested module imports,
// so a successful build proves the whole path resolves the replace directive
// with the build directory set as the working directory rather than the
// repository root.
func makeNestedModuleRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"), []byte("package fixture\n\nfunc Greeting() string { return \"hi\" }\n"), 0o600))

	cmdDir := filepath.Join(repo, "cmd", "tool")
	require.NoError(t, os.MkdirAll(cmdDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cmdDir, "go.mod"), []byte("module test.local/fixture-cmd\n\ngo 1.26\n\nrequire test.local/fixture v0.0.0\n\nreplace test.local/fixture => ../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\n\t\"test.local/fixture\"\n)\n\nfunc main() { fmt.Println(fixture.Greeting()) }\n"), 0o600))

	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

// writeNestedModuleManifest is writeManifest with target.build.directory set
// to the nested module's repository-relative path.
func writeNestedModuleManifest(t *testing.T, dir string) string {
	t.Helper()
	accept := true
	document := map[string]any{
		"version": "v1", "name": "fixture", "target": map[string]any{"repository": "local", "build": map[string]any{"directory": "cmd/tool", "package": ".", "binary": "tool"}, "command": []string{}},
		"workloads": map[string]any{"seeds": []any{map[string]any{"id": "fixture", "name": "fixture", "tier": "representative", "args": []string{}, "provenance": "test"}}, "discovery": map[string]any{"enabled": false, "sources": []string{}, "strategies": []string{}, "seed": 1, "max_cases": 1, "max_depth": 1}, "tiers": map[string]any{
			"representative": map[string]any{"weight": 1.0, "acceptance_eligible": true},
			"plausible":      map[string]any{"weight": 0.5, "acceptance_eligible": false},
			"stress":         map[string]any{"weight": 0.0, "acceptance_eligible": false},
		}},
		"sandbox":             map[string]any{"network": "deny", "filesystem": map[string]any{"read": "repo_and_assets", "write": "temp_only"}, "environment": map[string]any{"allow": []string{}, "passthrough": []string{}}, "max_processes": 1},
		"normalization":       map[string]any{"stdout": map[string]any{"mode": "exact"}, "stderr": map[string]any{"mode": "exact"}, "files": []any{}},
		"performance":         map[string]any{"primary_metric": "wall_time_ns", "minimum_improvement_percent": 3, "maximum_guardrail_regression_percent": 2, "statistical_support_required": accept, "guardrails": []any{}},
		"campaign":            map[string]any{"max_duration": "1m", "max_candidate_patches": 1, "max_concurrent_candidates": 1, "stop_after_failures": 1, "discovery_stall_timeout": "30s", "per_command_timeout_multiple": 2, "minimum_command_timeout": "10s"},
		"optimization_policy": "idiomatic",
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	path := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestBuildUsesNestedModuleDirectory is the engine-level proof for ADR 0023:
// a target whose CLI is a nested Go module builds correctly end to end
// (manifest -> engine -> toolchain), with go build running from the nested
// module's directory rather than the repository root. Before the change,
// this failed with "main module ... does not contain package .../cmd/tool",
// the exact error ADR 0023 exists to fix.
func TestBuildUsesNestedModuleDirectory(t *testing.T) {
	repo := makeNestedModuleRepository(t)
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeNestedModuleManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	require.Equal(t, "cmd/tool", engine.state.Manifest.Target.Build.Directory)
	require.NoError(t, engine.build(context.Background()))
	require.FileExists(t, engine.state.BinaryPath)
	require.FileExists(t, engine.state.DiscoveryBinaryPath)
}
