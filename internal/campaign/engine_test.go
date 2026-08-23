package campaign

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/profile"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

func TestCampaignRunsPersistsReportsAndResumes(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bubblewrap is required for local Linux isolation")
		}
	}
	repo := makeRepository(t)
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	manifestPath := writeManifest(t, t.TempDir())
	engine, err := Create(context.Background(), Options{Repository: repo, ManifestPath: manifestPath, CampaignDir: campaignDir, TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	require.NoError(t, engine.Run(context.Background()))
	state := engine.State()
	require.Equal(t, StatusCompleted, state.Status)
	require.Len(t, state.Runs, 1)
	require.NoError(t, engine.Close())

	for _, name := range []string{DatabaseName, "report.json", "report.md"} {
		_, err := os.Stat(filepath.Join(campaignDir, name))
		require.NoError(t, err)
	}
	status := git(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
	require.Empty(t, status)

	resumed, err := Resume(campaignDir, nil)
	require.NoError(t, err)
	require.NoError(t, resumed.Run(context.Background()))
	require.Len(t, resumed.State().Runs, 1, "completed workload must not repeat")
	require.NoError(t, resumed.Close())
}

func TestCreateRejectsDirtyRepository(t *testing.T) {
	repo := makeRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "untracked"), []byte("x"), 0o600))
	_, err := Create(context.Background(), Options{Repository: repo, ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign")})
	require.ErrorContains(t, err, "must be clean")
}

func TestRunADKFullGraphWithDeterministicAgents(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{Repository: repo, ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	require.NoError(t, engine.Run(context.Background()))
	defer engine.Close()
	static := func(name string, value any) adkagent.Agent {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		var output any
		require.NoError(t, json.Unmarshal(data, &output))
		a, err := adkagent.New(adkagent.Config{Name: name, Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				event := session.NewEvent(ctx, ctx.InvocationID())
				event.Output = output
				yield(event, nil)
			}
		}})
		require.NoError(t, err)
		return a
	}
	roles := agents.Set{
		Coordinator: static("coordinator", agents.CoordinatorResult{Objective: "test", NextExperiment: "test"}),
		Explorer:    static("explorer", agents.ExplorerResult{EntryPoints: []string{"main"}, Proposals: []agents.WorkloadProposal{{Name: "test", Tier: "plausible", Provenance: "test", ExpectedValid: true}}}),
		Analyst:     static("analyst", agents.AnalystResult{CandidateHypotheses: []string{"test"}}),
		Optimizer:   static("optimizer", agents.OptimizerResult{Hypothesis: "test", Patch: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n"}),
		Reviewer:    static("reviewer", agents.ReviewerResult{Proceed: true}),
	}
	result, err := engine.RunADK(context.Background(), roles, orchestrator.Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Second, AgentTimeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, engine.State().ID, result.CampaignID)
	require.Equal(t, 1, result.CandidatesTried)
	require.Equal(t, domain.DecisionRejected, result.FinalEvaluation.Decision)
	events, err := engine.store.Events()
	require.NoError(t, err)
	var saw bool
	for _, event := range events {
		if event.Type == "adk_completed" {
			saw = true
		}
	}
	require.True(t, saw)
}

func makeRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test.local/fixture\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nimport (\"fmt\"; \"os\")\nfunc main(){ b,_:=os.ReadFile(\"fixture.txt\"); fmt.Printf(\"%s\", b) }\n"), 0o600))
	git(t, repo, "init")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	return repo
}

func writeManifest(t *testing.T, dir string) string {
	t.Helper()
	accept := true
	document := map[string]any{
		"version": "v1", "name": "fixture", "target": map[string]any{"repository": "local", "build": map[string]any{"package": ".", "binary": "fixture"}, "command": []string{}},
		"workloads": map[string]any{"seeds": []any{map[string]any{"id": "fixture", "name": "fixture", "tier": "representative", "args": []string{}, "files": []any{map[string]any{"path": "fixture.txt", "content": "hello\n"}}, "provenance": "test"}}, "discovery": map[string]any{"enabled": false, "sources": []string{}, "strategies": []string{}, "seed": 1, "max_cases": 1, "max_depth": 1}, "tiers": map[string]any{
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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}

func TestHotFunctionNamesSkipsRuntimeAndDeduplicates(t *testing.T) {
	functions := []profile.Function{
		{Name: "runtime.schedule"},
		{Name: " main.handle "},
		{Name: "main.handle"},
		{Name: ""},
		{Name: "main.parse"},
	}
	got := hotFunctionNames(functions, 15)
	require.Equal(t, []string{"main.handle", "main.parse"}, got)
	require.Empty(t, hotFunctionNames(nil, 15))
	capped := hotFunctionNames([]profile.Function{{Name: "a"}, {Name: "b"}}, 1)
	require.Equal(t, []string{"a"}, capped)
}
