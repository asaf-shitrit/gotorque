// Package campaign composes local repository inspection, builds, workload
// execution, durable evidence, and reporting into the in-process campaign
// engine shared by command surfaces.
package campaign

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/profile"
	"example.com/gotorque/internal/runner"
	"example.com/gotorque/internal/toolchain"
)

const DatabaseName = "campaign.db"

type Status string

const (
	StatusPending     Status = "pending"
	StatusRunning     Status = "running"
	StatusInterrupted Status = "interrupted"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
)

type Environment struct {
	Authority     string            `json:"authority"`
	OS            string            `json:"os"`
	Architecture  string            `json:"architecture"`
	CPU           string            `json:"cpu"`
	GoVersion     string            `json:"go_version"`
	Revision      string            `json:"revision"`
	BuildFlags    []string          `json:"build_flags"`
	CI            bool              `json:"ci"`
	CIEnvironment map[string]string `json:"ci_environment,omitempty"`
}

type Inventory struct {
	Packages []string `json:"packages"`
	Commands []string `json:"commands"`
}

// CandidateRecord persists one evaluated model proposal with the policy
// verdict and measurement evidence, so reports can explain every decision.
type CandidateRecord struct {
	Attempt     int                       `json:"attempt"`
	CandidateID string                    `json:"candidate_id"`
	Hypothesis  string                    `json:"hypothesis"`
	PatchPath   string                    `json:"patch_path,omitempty"`
	Summary     string                    `json:"summary,omitempty"`
	Decision    domain.Decision           `json:"decision"`
	Reasons     []string                  `json:"reasons,omitempty"`
	Comparisons []domain.MetricComparison `json:"comparisons,omitempty"`
	Accepted    bool                      `json:"accepted,omitempty"`
	// BenchstatOutput holds trimmed raw benchstat output for workloads where
	// benchstat refined the wall-time comparison; empty when unavailable.
	BenchstatOutput string                   `json:"benchstat_output,omitempty"`
	Samples         []domain.WorkloadSamples `json:"samples,omitempty"`
	// PgoComparisons and PgoNote carry the informational PGO lane results
	// (both sides built with the same pprof CPU profile). They never change
	// the recorded decision; see runPgoLane.
	PgoComparisons []domain.MetricComparison `json:"pgo_comparisons,omitempty"`
	PgoNote        string                    `json:"pgo_note,omitempty"`
}

// RoleUsageSnapshot is persisted per-role model token usage for one ADK run.
type RoleUsageSnapshot struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type State struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Directory    string `json:"directory"`
	Repository   string `json:"repository"`
	ManifestPath string `json:"manifest_path"`
	ADKMode      string `json:"adk_mode,omitempty"`

	CandidateRecords            []CandidateRecord `json:"candidate_records,omitempty"`
	Manifest                    manifest.Manifest `json:"manifest"`
	Status                      Status            `json:"status"`
	StartedAt                   time.Time         `json:"started_at"`
	UpdatedAt                   time.Time         `json:"updated_at"`
	CompletedAt                 *time.Time        `json:"completed_at,omitempty"`
	Environment                 Environment       `json:"environment"`
	Inventory                   Inventory         `json:"inventory"`
	BuildID                     string            `json:"build_id,omitempty"`
	BinaryPath                  string            `json:"binary_path,omitempty"`
	DiscoveryBuildID            string            `json:"discovery_build_id,omitempty"`
	DiscoveryBinaryPath         string            `json:"discovery_binary_path,omitempty"`
	DiscoveryHotFunctions       []string          `json:"discovery_hot_functions,omitempty"`
	DiscoveryProfileSummaryPath string            `json:"discovery_profile_summary_path,omitempty"`
	// PGOProfilePath points at the raw pprof-format CPU profile produced by
	// benchmark-based discovery (profiles/bench-cpu.pb.gz), or is empty when
	// only a non-pprof sampler report exists. Only this file may seed the
	// informational PGO lane, because -pgo requires pprof input.
	PGOProfilePath    string             `json:"pgo_profile_path,omitempty"`
	Runs              []domain.RunResult `json:"runs,omitempty"`
	CompletedSteps    map[string]bool    `json:"completed_steps"`
	StopReason        string             `json:"stop_reason,omitempty"`
	Error             string             `json:"error,omitempty"`
	LocalIsolation    bool               `json:"local_isolation"`
	DependencyDigests map[string]string  `json:"dependency_digests,omitempty"`
	// TokenUsage holds per-role model token totals collected during ADK runs.
	TokenUsage map[string]RoleUsageSnapshot `json:"token_usage,omitempty"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
	Data    any       `json:"data,omitempty"`
}

type Options struct {
	Repository                    string
	ManifestPath                  string
	CampaignDir                   string
	Progress                      io.Writer
	Now                           func() time.Time
	TestingUnsafeDisableIsolation bool
	ADKAgents                     *agents.Set
	ADKConfig                     *orchestrator.Config
}

type Engine struct {
	dir       string
	store     *Store
	state     State
	toolchain *toolchain.Toolchain
	runner    *runner.Runner
	progress  io.Writer
	now       func() time.Time
	adkAgents *agents.Set
	adkConfig orchestrator.Config
}

func Create(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	repo, err := filepath.Abs(opts.Repository)
	if err != nil {
		return nil, fmt.Errorf("resolve repository: %w", err)
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repository: %w", err)
	}
	manifestPath, err := filepath.Abs(opts.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest: %w", err)
	}
	m, err := manifest.LoadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	tc := toolchain.New(toolchain.Options{})
	status, err := tc.GitStatus(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("verify Git repository: %w", err)
	}
	if len(status.Stdout) != 0 {
		return nil, fmt.Errorf("repository working tree must be clean (tracked and untracked files):\n%s", status.Stdout)
	}
	revisionResult, err := tc.GitRevision(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("read source revision: %w", err)
	}
	revision := strings.TrimSpace(string(revisionResult.Stdout))
	goResult, err := tc.GoVersion(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("verify local Go toolchain: %w", err)
	}
	now := opts.Now()
	id := stableID("campaign", revision, m.Name, now.Format(time.RFC3339Nano))
	dir := opts.CampaignDir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user cache: %w", err)
		}
		dir = filepath.Join(cache, "gotorque", "campaigns", id)
	} else if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	if inside, relErr := filepath.Rel(repo, dir); relErr == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return nil, errors.New("campaign directory must be outside the canonical repository")
	}
	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) != 0 {
		return nil, fmt.Errorf("campaign directory %q is not empty; use --resume", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	if err != nil {
		return nil, err
	}
	state := State{Version: 1, ID: id, Directory: dir, Repository: repo, ManifestPath: manifestPath, Manifest: m,
		Status: StatusPending, StartedAt: now, UpdatedAt: now, CompletedSteps: map[string]bool{}, LocalIsolation: !opts.TestingUnsafeDisableIsolation,
		Environment: Environment{Authority: authority(), OS: runtime.GOOS, Architecture: runtime.GOARCH, CPU: cpuName(), GoVersion: strings.TrimSpace(string(goResult.Stdout)), Revision: revision, BuildFlags: []string{"-mod=readonly", "-trimpath"}, CI: os.Getenv("CI") != "", CIEnvironment: ciEnvironment()},
	}
	state.DependencyDigests, err = dependencyDigests(repo)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	e, err := compose(dir, store, state, opts.Progress, opts.Now)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	e.adkAgents = opts.ADKAgents
	if opts.ADKConfig != nil {
		e.adkConfig = *opts.ADKConfig
	}
	if opts.ADKAgents != nil {
		e.state.ADKMode = "live"
	}
	if err := e.saveEvent("campaign_created", "campaign initialized", nil); err != nil {
		_ = store.Close()
		return nil, err
	}
	return e, nil
}

func Resume(dir string, progress io.Writer) (*Engine, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(abs, DatabaseName))
	if err != nil {
		return nil, err
	}
	state, err := store.Load()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return compose(abs, store, state, progress, func() time.Time { return time.Now().UTC() })
}

func compose(dir string, store *Store, state State, progress io.Writer, now func() time.Time) (*Engine, error) {
	artifacts, err := runner.NewArtifactStore(filepath.Join(dir, "artifacts"))
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Options{Artifacts: artifacts, SandboxRoot: filepath.Join(dir, "sandboxes"), LocalIsolation: state.LocalIsolation})
	if err != nil {
		return nil, err
	}
	if progress == nil {
		progress = io.Discard
	}
	return &Engine{dir: dir, store: store, state: state, toolchain: toolchain.New(toolchain.Options{}), runner: r, progress: progress, now: now}, nil
}

func (e *Engine) Close() error { return e.store.Close() }
func (e *Engine) State() State { return e.state }

func (e *Engine) Run(ctx context.Context) (err error) {
	if e.state.Status == StatusCompleted {
		return nil
	}
	deadline := e.state.Manifest.Campaign.MaxDuration.Duration()
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	e.state.Status, e.state.Error = StatusRunning, ""
	if err = e.saveEvent("campaign_started", "campaign running", nil); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			e.state.UpdatedAt = e.now()
			e.state.Error = err.Error()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				e.state.Status = StatusInterrupted
			} else {
				e.state.Status = StatusFailed
			}
			_ = e.saveEvent("campaign_stopped", e.state.Error, nil)
		}
	}()
	if !e.state.CompletedSteps["inspect"] {
		if err = e.inspect(ctx); err != nil {
			return err
		}
		e.state.CompletedSteps["inspect"] = true
		if err = e.saveEvent("repository_inspected", fmt.Sprintf("discovered %d packages", len(e.state.Inventory.Packages)), e.state.Inventory); err != nil {
			return err
		}
	}
	if !e.state.CompletedSteps["build"] {
		if err = e.build(ctx); err != nil {
			return err
		}
		e.state.CompletedSteps["build"] = true
		if err = e.saveEvent("baseline_built", "release-equivalent baseline built", map[string]string{"build_id": e.state.BuildID}); err != nil {
			return err
		}
	}
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		step := "workload:" + seed.ID
		if e.state.CompletedSteps[step] {
			continue
		}
		if err = e.runSeed(ctx, seed); err != nil {
			return err
		}
		e.state.CompletedSteps[step] = true
		if err = e.saveEvent("workload_completed", seed.Name, map[string]string{"workload_id": seed.ID}); err != nil {
			return err
		}
	}
	if !e.state.CompletedSteps["discovery_profile"] {
		if err = e.collectDiscoveryProfile(ctx); err != nil {
			return err
		}
		e.state.CompletedSteps["discovery_profile"] = true
		if err = e.saveEvent("discovery_profile_completed", fmt.Sprintf("measured %d hot functions", len(e.state.DiscoveryHotFunctions)), nil); err != nil {
			return err
		}
	}
	if err = e.verifyClean(ctx); err != nil {
		return err
	}
	stopReason := "baseline discovery complete; no model candidate requested"
	if e.adkAgents != nil {
		result, err := e.RunADK(ctx, *e.adkAgents, e.adkConfig)
		if err != nil {
			return err
		}
		if strings.TrimSpace(result.StopReason) != "" {
			stopReason = result.StopReason
		}
	}
	now := e.now()
	e.state.Status, e.state.CompletedAt, e.state.StopReason = StatusCompleted, &now, stopReason
	e.state.CompletedSteps["complete"] = true
	if err = e.saveEvent("campaign_completed", e.state.StopReason, nil); err != nil {
		return err
	}
	return WriteReports(e.dir, e.state)
}

func (e *Engine) inspect(ctx context.Context) error {
	result, err := e.toolchain.GoList(ctx, e.state.Repository)
	if err != nil {
		return fmt.Errorf("inventory Go packages: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(result.Stdout)))
	var packages, commands []string
	for {
		var item struct{ ImportPath, Name string }
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode go list: %w", err)
		}
		packages = append(packages, item.ImportPath)
		if item.Name == "main" {
			commands = append(commands, item.ImportPath)
		}
	}
	sort.Strings(packages)
	sort.Strings(commands)
	e.state.Inventory = Inventory{Packages: packages, Commands: commands}
	return nil
}

func (e *Engine) build(ctx context.Context) error {
	binDir := filepath.Join(e.dir, "builds")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	e.state.BuildID = stableID("build", e.state.Environment.Revision, e.state.Manifest.Target.Build.Package, strings.Join(e.state.Environment.BuildFlags, " "))
	e.state.BinaryPath = filepath.Join(binDir, e.state.BuildID+"-"+filepath.Base(e.state.Manifest.Target.Build.Binary))
	_, err := e.toolchain.Build(ctx, toolchain.BuildRequest{Repository: e.state.Repository, Target: e.state.Manifest.Target.Build.Package, Output: e.state.BinaryPath, Env: []string{"GOTOOLCHAIN=local"}})
	if err != nil {
		return fmt.Errorf("build baseline: %w", err)
	}
	e.state.DiscoveryBuildID = stableID("build", e.state.Environment.Revision, e.state.Manifest.Target.Build.Package, "coverage")
	e.state.DiscoveryBinaryPath = filepath.Join(binDir, e.state.DiscoveryBuildID+"-coverage-"+filepath.Base(e.state.Manifest.Target.Build.Binary))
	_, err = e.toolchain.Build(ctx, toolchain.BuildRequest{Repository: e.state.Repository, Target: e.state.Manifest.Target.Build.Package, Output: e.state.DiscoveryBinaryPath, Cover: true, Env: []string{"GOTOOLCHAIN=local"}})
	if err != nil {
		return fmt.Errorf("build coverage baseline: %w", err)
	}
	return e.verifyClean(ctx)
}

func (e *Engine) runSeed(ctx context.Context, seed manifest.SeedWorkload) error {
	fixtures := make(map[string][]byte, len(seed.Files))
	for _, f := range seed.Files {
		fixtures[f.Path] = []byte(f.Content)
	}
	timeout := seed.Timeout.Duration()
	if timeout == 0 {
		timeout = e.state.Manifest.Campaign.MinimumCommandTimeout.Duration()
	}
	wid := stableID("workload", e.state.ID, seed.ID)
	workload := domain.Workload{ID: wid, Name: seed.Name, Tier: seed.Tier, Command: domain.Command{Path: e.state.DiscoveryBinaryPath, Args: append(append([]string{}, e.state.Manifest.Target.Command...), seed.Args...)}, Timeout: timeout, Provenance: seed.Provenance, Description: seed.Description}
	result, runErr := e.runner.Run(ctx, runner.RunRequest{Build: runner.Build{ID: e.state.DiscoveryBuildID, BinaryPath: e.state.DiscoveryBinaryPath}, Workload: workload, Mode: domain.RunModeDiscovery, NetworkAllowed: !e.state.LocalIsolation, FilesystemAllowed: !e.state.LocalIsolation, AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"}, Stdin: []byte(seed.Stdin), Fixtures: fixtures})
	result.ID = stableID("run", e.state.DiscoveryBuildID, wid, "baseline")
	e.state.Runs = append(e.state.Runs, result)
	if runErr != nil {
		return fmt.Errorf("baseline workload %q was invalid: %w", seed.ID, runErr)
	}
	return nil
}

// collectDiscoveryProfile is best-effort: any failure records an event and
// leaves discovery evidence empty rather than failing the campaign. It first
// tries Go benchmark CPU profiles; targets without benchmarks (or failing
// profile summarization) fall back to sampling the built target binary
// directly with platform tools, so every target gets hot-path evidence.
func (e *Engine) collectDiscoveryProfile(ctx context.Context) error {
	var benchErr error
	if err := e.profileHotFunctions(ctx); err != nil {
		benchErr = err
	} else {
		return nil
	}
	if err := e.sampleTargetProfile(ctx); err != nil {
		_ = e.saveEvent("discovery_profile_skipped", fmt.Sprintf("benchmark CPU profile unavailable (%s); direct target sampling unavailable (%s)", benchErr.Error(), err.Error()), nil)
	}
	return nil
}

// sampleTargetProfile runs the first representative seed workload against the
// release baseline binary under the platform sampler (macOS `sample`, Linux
// `perf`) and records the hottest frames as discovery evidence. The raw
// sampler report is preserved under profile-sample/ in the campaign dir.
// Strictly best-effort: any failure is returned for a skipped event.
func (e *Engine) sampleTargetProfile(ctx context.Context) error {
	if e.state.BinaryPath == "" {
		return errors.New("no baseline binary")
	}
	if info, statErr := os.Stat(e.state.BinaryPath); statErr != nil || info.IsDir() {
		return errors.New("baseline binary missing on disk")
	}
	if len(e.state.Manifest.Workloads.Seeds) == 0 {
		return errors.New("manifest defines no seed workloads to sample")
	}
	seed := e.state.Manifest.Workloads.Seeds[0]
	fixtures := make(map[string][]byte, len(seed.Files))
	for _, f := range seed.Files {
		fixtures[f.Path] = []byte(f.Content)
	}
	// The sampler needs the target to stay alive for the whole window.
	// Tiny seed inputs finish in milliseconds, so amplify stdin by
	// repetition (capped at 32 MiB) until the workload is long-lived.
	stdin := []byte(seed.Stdin)
	if len(stdin) > 0 && len(stdin) < 32<<20 {
		amplified := make([]byte, 0, 32<<20)
		for len(amplified) < 16<<20 {
			amplified = append(amplified, stdin...)
		}
		stdin = amplified
	}
	outDir := filepath.Join(e.dir, "profile-sample")
	result, err := profile.SampleTargetProfile(ctx, profile.SampleTarget{
		BinaryPath: e.state.BinaryPath,
		Args:       append(append([]string{}, e.state.Manifest.Target.Command...), seed.Args...),
		Stdin:      stdin,
		Fixtures:   fixtures,
		Duration:   4 * time.Second,
		OutputPath: filepath.Join(outDir, "sample-report.txt"),
	})
	if err != nil {
		return err
	}
	e.state.DiscoveryHotFunctions = e.resolveHotLocations(ctx, "", hotFunctionNames(result.Functions, 15))
	e.state.DiscoveryProfileSummaryPath = result.RawReport
	return nil
}

// profileHotFunctions runs the target package's benchmarks under a CPU
// profile, then summarizes the top functions via go tool pprof.
func (e *Engine) profileHotFunctions(ctx context.Context) error {
	dir := filepath.Join(e.dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cpuProfile := filepath.Join(dir, "bench-cpu.pb.gz")
	result, err := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: e.state.Repository, Packages: []string{e.state.Manifest.Target.Build.Package}, Bench: ".", Cpuprofile: cpuProfile, Env: []string{"GOTOOLCHAIN=local"}})
	if err != nil {
		return err
	}
	if !strings.Contains(string(result.Stdout), "Benchmark") {
		return errors.New("target package has no benchmark functions")
	}
	artifacts, err := runner.NewArtifactStore(filepath.Join(e.dir, "artifacts"))
	if err != nil {
		return err
	}
	summary, err := profile.Collector{Toolchain: e.toolchain, Artifacts: artifacts}.SummarizePprof(ctx, cpuProfile, 15)
	if err != nil {
		return fmt.Errorf("summarize benchmark CPU profile: %w", err)
	}
	names := hotFunctionNames(summary.Functions, 15)
	e.state.DiscoveryHotFunctions = e.resolveHotLocations(ctx, cpuProfile, names)
	e.state.DiscoveryProfileSummaryPath = summary.RawReport
	e.state.PGOProfilePath = cpuProfile
	return nil
}

// resolveHotLocations annotates hot function names with repository-relative
// source positions. Preferred source is `go tool pprof -list` over the
// benchmark profile (exact file and sampled line); the fallback searches the
// repository for the declaration. Unresolvable functions keep their bare
// names so downstream consumers never lose entries.
func (e *Engine) resolveHotLocations(ctx context.Context, cpuProfile string, names []string) []string {
	locations := make([]string, 0, len(names))
	for _, name := range names {
		entry := name
		if cpuProfile != "" {
			if result, err := e.toolchain.PprofList(ctx, name, cpuProfile); err == nil {
				if path, line, ok := profile.ParsePprofList(string(result.Stdout)); ok {
					entry = profile.HotLocation{Function: name, Path: path, Line: line}.Location()
				}
			}
		}
		if entry == name && !strings.Contains(name, ":") {
			if path, line, ok := profile.FindFunctionInRepo(e.state.Repository, name); ok {
				entry = profile.HotLocation{Function: name, Path: path, Line: line}.Location()
			}
		}
		locations = append(locations, entry)
	}
	return locations
}

// hotFunctionNames extracts deduplicated function names from a parsed pprof
// top summary, skipping runtime frames that never belong to the target.
func hotFunctionNames(functions []profile.Function, max int) []string {
	names := make([]string, 0, max)
	seen := map[string]bool{}
	for _, fn := range functions {
		name := strings.TrimSpace(fn.Name)
		if name == "" || strings.HasPrefix(name, "runtime.") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		if len(names) == max {
			break
		}
	}
	return names
}

func (e *Engine) verifyClean(ctx context.Context) error {
	status, err := e.toolchain.GitStatus(ctx, e.state.Repository)
	if err != nil {
		return err
	}
	if len(status.Stdout) != 0 {
		return fmt.Errorf("canonical checkout changed during campaign:\n%s", status.Stdout)
	}
	rev, err := e.toolchain.GitRevision(ctx, e.state.Repository)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(rev.Stdout)) != e.state.Environment.Revision {
		return errors.New("canonical checkout revision changed during campaign")
	}
	digests, err := dependencyDigests(e.state.Repository)
	if err != nil {
		return err
	}
	if !maps.Equal(digests, e.state.DependencyDigests) {
		return errors.New("go.mod, go.sum, or vendored dependency content changed during campaign")
	}
	return nil
}

func dependencyDigests(repository string) (map[string]string, error) {
	paths := []string{"go.mod", "go.sum"}
	vendor := filepath.Join(repository, "vendor")
	if info, err := os.Stat(vendor); err == nil && info.IsDir() {
		err = filepath.WalkDir(vendor, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type().IsRegular() {
				rel, err := filepath.Rel(repository, path)
				if err != nil {
					return err
				}
				paths = append(paths, rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	result := make(map[string]string, len(paths))
	for _, rel := range paths {
		path := filepath.Join(repository, rel)
		digest, err := runner.DigestFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[filepath.ToSlash(rel)] = digest
	}
	return result, nil
}

func (e *Engine) saveEvent(kind, message string, data any) error {
	now := e.now()
	e.state.UpdatedAt = now
	if err := e.store.Save(e.state); err != nil {
		return err
	}
	if err := e.store.Append(Event{Time: now, Type: kind, Message: message, Data: data}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.progress, "[%s] %s\n", kind, message)
	return nil
}

func stableID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func authority() string {
	if runtime.GOOS == "linux" {
		return "authoritative"
	}
	return "provisional"
}

func cpuName() string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return runtime.GOARCH
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if key, value, ok := strings.Cut(line, ":"); ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware") {
			return strings.TrimSpace(value)
		}
	}
	return runtime.GOARCH
}

func ciEnvironment() map[string]string {
	result := map[string]string{}
	for _, key := range []string{"CI", "GITHUB_ACTIONS", "GITHUB_RUN_ID", "GITLAB_CI", "BUILD_ID", "BUILDKITE"} {
		if value := os.Getenv(key); value != "" {
			result[key] = value
		}
	}
	return result
}

// SetADK attaches model agents to a resumed campaign. Mid-workflow stops
// lose the in-memory agent clients, so resume flows must re-supply them
// before Run re-enters the ADK graph.
func (e *Engine) SetADK(roleSet *agents.Set, cfg *orchestrator.Config) {
	e.adkAgents = roleSet
	if cfg != nil {
		e.adkConfig = *cfg
	}
}
