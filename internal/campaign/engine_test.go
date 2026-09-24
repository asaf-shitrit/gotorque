package campaign

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
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
	defer func() { _ = engine.Close() }()
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

// staticAgent replays a fixed decoded payload as one agent turn, standing in
// for a model so campaign-level graph runs stay deterministic and offline.
func staticAgent(t *testing.T, name string, value any) adkagent.Agent {
	t.Helper()
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

// rejectingRoles proposes a patch that cannot apply, so every candidate the
// graph evaluates is rejected deterministically without a build or a model.
func rejectingRoles(t *testing.T) agents.Set {
	t.Helper()
	return agents.Set{
		Coordinator: staticAgent(t, "coordinator", agents.CoordinatorResult{Objective: "test", NextExperiment: "test"}),
		Explorer:    staticAgent(t, "explorer", agents.ExplorerResult{EntryPoints: []string{"main"}}),
		Analyst:     staticAgent(t, "analyst", agents.AnalystResult{CandidateHypotheses: []string{"test"}}),
		Optimizer:   staticAgent(t, "optimizer", agents.OptimizerResult{Hypothesis: "test", Patch: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n"}),
		Reviewer:    staticAgent(t, "reviewer", agents.ReviewerResult{Proceed: true}),
	}
}

// TestConsecutiveFailureBoundSurvivesResume covers the defect that let an
// interrupted campaign evaluate 12 rejected candidates under
// stop_after_failures=4: the tally lived only in the in-process orchestrator
// run, so every `optimize --resume` restarted it at zero.
func TestConsecutiveFailureBoundSurvivesResume(t *testing.T) {
	repo := makeRepository(t)
	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{Repository: repo, ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: campaignDir, TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	require.NoError(t, engine.Run(context.Background()))

	roles := rejectingRoles(t)
	bounded := func(maxCandidates int) orchestrator.Config {
		return orchestrator.Config{MaxCandidates: maxCandidates, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Minute, AgentTimeout: time.Minute}
	}
	// First process: stopped by its candidate ceiling with two of the four
	// allowed consecutive failures already spent, as an OS interrupt would.
	first, err := engine.RunADK(context.Background(), roles, bounded(2))
	require.NoError(t, err)
	require.Equal(t, 2, first.CandidatesTried)
	require.Equal(t, 2, engine.State().ConsecutiveFailures)
	require.NoError(t, engine.Close())

	resumed, err := Resume(campaignDir, nil)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	require.Equal(t, 2, resumed.State().ConsecutiveFailures, "tally must survive the process boundary")

	// Second process: a slack candidate ceiling, so only the carried-in tally
	// can stop it. Before the fix this ran the full four rejections again.
	second, err := resumed.RunADK(context.Background(), roles, bounded(12))
	require.NoError(t, err)
	require.Equal(t, 2, second.CandidatesTried, "resume must spend only the campaign's remaining allowance")
	require.Equal(t, "consecutive rejection/inconclusive limit reached", second.StopReason)
	require.Equal(t, 4, resumed.State().ConsecutiveFailures)
}

func TestCampaignDeadlineSpendsOnlyTheRemainingBudget(t *testing.T) {
	tests := []struct {
		name         string
		budget       time.Duration
		elapsed      time.Duration
		wantRemains  time.Duration
		wantDeadline bool
	}{
		{name: "fresh campaign gets the whole budget", budget: time.Minute, wantRemains: time.Minute, wantDeadline: true},
		{name: "resume gets what the campaign has left", budget: time.Minute, elapsed: 45 * time.Second, wantRemains: 15 * time.Second, wantDeadline: true},
		{name: "exhausted budget expires immediately", budget: time.Minute, elapsed: 90 * time.Second, wantDeadline: true},
		{name: "absent budget leaves the context alone", elapsed: time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := &Engine{state: State{
				Manifest:       manifest.Manifest{Campaign: manifest.CampaignLimits{MaxDuration: manifest.Duration(tc.budget)}},
				ElapsedRunTime: tc.elapsed,
			}}
			ctx, cancel := engine.withCampaignDeadline(context.Background())
			if cancel != nil {
				defer cancel()
			}
			deadline, ok := ctx.Deadline()
			require.Equal(t, tc.wantDeadline, ok)
			if !tc.wantDeadline {
				return
			}
			require.InDelta(t, tc.wantRemains.Seconds(), time.Until(deadline).Seconds(), 1)
		})
	}
}

// suspendedClock is a wall clock a test can jump forward. It stands in for a
// machine suspending mid-campaign: time.Now leaps ahead while the monotonic
// clock Go timers run on does not move at all.
type suspendedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *suspendedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *suspendedClock) suspend(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// suspendingAgent replays a fixed payload like staticAgent, but on its first
// turn it jumps the clock past the campaign budget and then waits to be
// cancelled. That puts the overrun where the real one happened -- inside a
// model call, with no engine code running to notice the budget is gone. The
// grace period caps the wait, so an unenforced bound fails the test rather
// than hanging it.
func suspendingAgent(t *testing.T, name string, value any, clock *suspendedClock, jump, grace time.Duration) adkagent.Agent {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	var output any
	require.NoError(t, json.Unmarshal(data, &output))
	var once sync.Once
	a, err := adkagent.New(adkagent.Config{Name: name, Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			once.Do(func() {
				clock.suspend(jump)
				select {
				case <-ctx.Done():
				case <-time.After(grace):
				}
			})
			event := session.NewEvent(ctx, ctx.InvocationID())
			event.Output = output
			yield(event, nil)
		}
	}})
	require.NoError(t, err)
	return a
}

// TestCampaignStopsWhenSuspensionSpendsTheDurationBudget covers the defect
// that let a 90-minute campaign run for three and a half hours: max_duration
// was enforced only by context.WithTimeout, whose timer runs on the monotonic
// clock, and that clock stops while the machine is suspended. ElapsedRunTime
// is measured with time.Now and kept charging the nap against the same
// budget, so the two clocks disagreed by the whole length of it.
func TestCampaignStopsWhenSuspensionSpendsTheDurationBudget(t *testing.T) {
	clock := &suspendedClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	roles := rejectingRoles(t)
	// The fixture manifest allows a minute, which the stub graph never spends
	// for real; only the simulated suspension can exhaust it.
	roles.Coordinator = suspendingAgent(t, "coordinator", agents.CoordinatorResult{Objective: "test", NextExperiment: "test"}, clock, 2*time.Minute, 5*time.Second)
	cfg := orchestrator.Config{MaxCandidates: 4, MaxConsecutiveFailures: 4, DeterministicTimeout: time.Minute, AgentTimeout: time.Minute}

	campaignDir := filepath.Join(t.TempDir(), "campaign")
	engine, err := Create(context.Background(), Options{
		Repository:                    makeRepository(t),
		ManifestPath:                  writeManifest(t, t.TempDir()),
		CampaignDir:                   campaignDir,
		TestingUnsafeDisableIsolation: true,
		Now:                           clock.Now,
		ADKAgents:                     &roles,
		ADKConfig:                     &cfg,
	})
	require.NoError(t, err)

	err = engine.Run(context.Background())
	require.ErrorIs(t, err, ErrDurationBudgetExhausted)
	state := engine.State()
	require.Equal(t, StatusInterrupted, state.Status)
	require.Contains(t, state.StopReason, "max_duration", "the stop must name the bound that ended the campaign")
	require.False(t, state.CompletedSteps["complete"], "an over-budget campaign must not be reported as finished")
	require.NoError(t, engine.Close())

	// Resuming a campaign with nothing left used to report whichever call the
	// expired context killed first -- "git status ...: context deadline
	// exceeded" -- naming an incidental command rather than the spent bound.
	resumed, err := Resume(campaignDir, nil)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	require.ErrorIs(t, resumed.Run(context.Background()), ErrDurationBudgetExhausted)
}

// TestCampaignDeadlineStopsOnWallClockOverrun pins the guard on its own: the
// campaign context must die once the wall clock says the budget is spent,
// even while the context's own timer still believes it has the whole hour.
func TestCampaignDeadlineStopsOnWallClockOverrun(t *testing.T) {
	clock := &suspendedClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	engine := &Engine{
		state:        State{Manifest: manifest.Manifest{Campaign: manifest.CampaignLimits{MaxDuration: manifest.Duration(time.Hour)}}},
		now:          clock.Now,
		runStartedAt: clock.Now(),
	}
	ctx, cancel := engine.withCampaignDeadline(context.Background())
	defer cancel()
	require.NoError(t, ctx.Err(), "an hour of budget must not expire the moment it is granted")

	clock.suspend(2 * time.Hour)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("campaign context outlived its wall-clock budget")
	}
	require.ErrorIs(t, context.Cause(ctx), ErrDurationBudgetExhausted)
}

func TestCampaignRunTimeAccumulatesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := func() time.Time { clock = clock.Add(10 * time.Second); return clock }
	// Persisted state round-trips through the manifest decoder, which refuses
	// the zero-value durations a bare State would carry.
	m, err := manifest.LoadFile(writeManifest(t, t.TempDir()))
	require.NoError(t, err)

	first, err := compose(dir, store, State{Manifest: m, CompletedSteps: map[string]bool{}}, nil, tick)
	require.NoError(t, err)
	first.startRunClock()
	require.NoError(t, first.saveEvent("test_event", "first", nil))
	require.NoError(t, first.saveEvent("test_event", "second", nil))
	require.Equal(t, 20*time.Second, first.state.ElapsedRunTime)

	persisted, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, 20*time.Second, persisted.ElapsedRunTime)

	second, err := compose(dir, store, persisted, nil, tick)
	require.NoError(t, err)
	second.startRunClock()
	require.NoError(t, second.saveEvent("test_event", "third", nil))
	require.Equal(t, 30*time.Second, second.state.ElapsedRunTime, "a resumed process continues the campaign total")
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
	capped := hotFunctionNames([]profile.Function{{Name: "main.a"}, {Name: "main.b"}}, 1)
	require.Equal(t, []string{"main.a"}, capped)
}

// TestBenchmarkProfilingLeavesTheCheckoutClean: go test keeps the test binary
// it profiled in the working directory, the canonical checkout, which then no
// longer matches its revision and stops the campaign. go-jsonnet, the first
// target with benchmarks, stopped that way before its first candidate.
func TestBenchmarkProfilingLeavesTheCheckoutClean(t *testing.T) {
	repo := makeRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "bench_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc BenchmarkSum(b *testing.B) {\n\tfor i := 0; i < b.N; i++ {\n\t\t_ = i * i\n\t}\n}\n"), 0o600))
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "benchmark")
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	profilePath, err := engine.benchmarkCPUProfile(context.Background())
	require.NoError(t, err)
	require.FileExists(t, profilePath)
	require.Empty(t, git(t, repo, "status", "--porcelain", "--untracked-files=all", "--ignored"), "profiling must not write into the checkout")
}

// TestAllocationProfilingLeavesTheCheckoutClean is the memprofile analogue of
// TestBenchmarkProfilingLeavesTheCheckoutClean: profileAllocations runs the
// same benchmarks a second time under -memprofile (ADR 0024), and must keep
// that test binary out of the canonical checkout too.
func TestAllocationProfilingLeavesTheCheckoutClean(t *testing.T) {
	repo := makeRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "bench_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc BenchmarkSum(b *testing.B) {\n\tfor i := 0; i < b.N; i++ {\n\t\t_ = make([]int, 8)\n\t}\n}\n"), 0o600))
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "benchmark")
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: writeManifest(t, t.TempDir()),
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	source := engine.profileAllocations(context.Background())
	require.Equal(t, "a benchmark alloc_space profile", source)
	require.NotEmpty(t, engine.state.DiscoveryAllocProfileSummaryPath)
	require.Empty(t, git(t, repo, "status", "--porcelain", "--untracked-files=all", "--ignored"), "allocation profiling must not write into the checkout")
}

func TestMergeAllocFirstPrefersAllocatorsWithoutDroppingCPUEvidence(t *testing.T) {
	cpu := []string{"a.go:1", "b.go:2", "c.go:3"}
	alloc := []string{"c.go:3", "d.go:4"}

	got := mergeAllocFirst(cpu, alloc, 4)
	require.Equal(t, []string{"c.go:3", "d.go:4", "a.go:1", "b.go:2"}, got, "allocators lead, deduplicated, then the rest of the CPU list")

	require.Equal(t, []string{"c.go:3", "d.go:4"}, mergeAllocFirst(cpu, alloc, 2), "budget still caps the merged list")
}
