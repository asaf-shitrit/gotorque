// Package campaign composes local repository inspection, builds, workload
// execution, durable evidence, and reporting into the in-process campaign
// engine shared by command surfaces.
package campaign

import (
	"bufio"
	"bytes"
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
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
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

// ErrDurationBudgetExhausted reports that a campaign stopped because the
// manifest's max_duration is spent. It exists so the stop can be recognised
// by callers and named in state, rather than inferred from whichever call
// happened to be in flight when the campaign context died.
var ErrDurationBudgetExhausted = errors.New("campaign max_duration exhausted")

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
	Attempt     int    `json:"attempt"`
	CandidateID string `json:"candidate_id"`
	Hypothesis  string `json:"hypothesis"`
	// Target is the function and cause code told the optimizer to attack, when
	// the analyst ranked causes.
	Target    *agents.Target `json:"target,omitempty"`
	PatchPath string         `json:"patch_path,omitempty"`
	// ReviewConcerns are the behaviour hazards the reviewer raised. They are
	// recorded for the reader and the next cycle; the verdict never reads them.
	ReviewConcerns []string                  `json:"review_concerns,omitempty"`
	Summary        string                    `json:"summary,omitempty"`
	Decision       domain.Decision           `json:"decision"`
	Reasons        []string                  `json:"reasons,omitempty"`
	Comparisons    []domain.MetricComparison `json:"comparisons,omitempty"`
	Accepted       bool                      `json:"accepted,omitempty"`
	// BenchstatOutput holds trimmed raw benchstat output for workloads where
	// benchstat refined the wall-time comparison; empty when unavailable.
	BenchstatOutput string                   `json:"benchstat_output,omitempty"`
	Samples         []domain.WorkloadSamples `json:"samples,omitempty"`
	// PgoComparisons and PgoNote carry the informational PGO lane results
	// (both sides built with the same pprof CPU profile). They never change
	// the recorded decision; see runPgoLane.
	PgoComparisons []domain.MetricComparison `json:"pgo_comparisons,omitempty"`
	PgoNote        string                    `json:"pgo_note,omitempty"`
	// ProposalRepair names the rewrite the optimizer's output needed before it
	// parsed, empty when it parsed as sent. A salvaged patch -- one cut off at
	// the output-token cap above all -- otherwise reads exactly like an
	// intended one. It never changes the recorded decision.
	ProposalRepair string `json:"proposal_repair,omitempty"`
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
	// Analyst is AnalystJev when the analyst role was Jev cause classification
	// rather than a model, so a report says which kind of analysis its
	// hypotheses came from and a resume can insist on the same one.
	Analyst string `json:"analyst,omitempty"`
	// Reviewer is ReviewerJev when the reviewer role was Jev behaviour-hazard
	// checks rather than a model.
	Reviewer string `json:"reviewer,omitempty"`
	// Explorer is ExplorerJev when discovery's extra workloads were generated
	// by code and judged by Jev instead of proposed by the explorer model.
	Explorer string `json:"explorer,omitempty"`
	// Tradeoff is the trade-off the campaign was started with. Manifest
	// already holds the performance block it produced, which is what every
	// verdict and a resume read; this records where that block came from.
	Tradeoff manifest.Tradeoff `json:"tradeoff,omitzero"`
	// SchemaVersion is stamped by WriteReports onto the artifact it writes, so a
	// report carries the shape it was written in. It stays zero for state that
	// predates versioning, which readers report rather than assume.
	SchemaVersion int `json:"schema_version,omitempty"`

	CandidateRecords []CandidateRecord `json:"candidate_records,omitempty"`
	// ConsecutiveFailures mirrors the orchestrator's run of rejected or
	// inconclusive candidates. The graph builds a fresh CampaignState every
	// time it is entered, so without a persisted tally each resume would
	// restart the manifest's stop_after_failures bound at zero and the bound
	// would only ever hold inside one process.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// ConsecutiveInconclusive mirrors the orchestrator's separate run of
	// inconclusive verdicts, used only when the manifest configures
	// stop_after_inconclusive. It is persisted for the same reason as the
	// failure tally: a resumed campaign must not restart the bound at zero.
	ConsecutiveInconclusive int `json:"consecutive_inconclusive,omitempty"`
	// DegradedRoles records agent nodes whose model call failed and were
	// absorbed: the campaign continued with an empty result, so a candidate may
	// be missing that role's output. Without this the cause lived only on the
	// process's stderr, where no report and no API consumer could read it.
	DegradedRoles []RoleDegradation `json:"degraded_roles,omitempty"`
	Manifest      manifest.Manifest `json:"manifest"`
	Status        Status            `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	// ElapsedRunTime is the wall time this campaign has spent actually
	// running, summed over every process that has worked on it. StartedAt
	// cannot stand in for it: a campaign is idle between an interruption and
	// its resume, and charging that idle time against max_duration would
	// expire any campaign resumed the next day.
	ElapsedRunTime        time.Duration `json:"elapsed_run_time,omitempty"`
	Environment           Environment   `json:"environment"`
	Inventory             Inventory     `json:"inventory"`
	BuildID               string        `json:"build_id,omitempty"`
	BinaryPath            string        `json:"binary_path,omitempty"`
	DiscoveryBuildID      string        `json:"discovery_build_id,omitempty"`
	DiscoveryBinaryPath   string        `json:"discovery_binary_path,omitempty"`
	DiscoveryHotFunctions []string      `json:"discovery_hot_functions,omitempty"`
	// DiscoveryWorkloads names the option variants discovery sampled next to
	// the first seed, with Jev's processing-mode judgment of each.
	DiscoveryWorkloads          []string `json:"discovery_workloads,omitempty"`
	DiscoveryProfileSummaryPath string   `json:"discovery_profile_summary_path,omitempty"`
	// DiscoveryProfileSource names which profile(s) chose
	// DiscoveryHotFunctions: a target sample, target benchmarks, or a target
	// sample/benchmark plus a benchmark alloc_space profile when the campaign's
	// objective is peak memory (ADR 0024). Written for the report, which
	// otherwise cannot say why a lean campaign's targets came out the way they
	// did.
	DiscoveryProfileSource string `json:"discovery_profile_source,omitempty"`
	// DiscoveryAllocProfileSummaryPath is the artifact path of the `go tool
	// pprof -top -alloc_space` report over the benchmark heap profile, set
	// only when the objective is peak memory and the module has benchmarks.
	DiscoveryAllocProfileSummaryPath string `json:"discovery_alloc_profile_summary_path,omitempty"`
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
	// BaselineTestFailures holds the tests that already fail on the unpatched
	// revision, keyed `package::Test`. The behavior gate only rejects a
	// candidate for failures absent from this set.
	BaselineTestFailures []string `json:"baseline_test_failures,omitempty"`
	// BaselineTestPasses holds the tests, subtests included, that pass on the
	// unpatched revision, keyed `package::Test`. A candidate must pass every
	// one of them: comparing failures alone never noticed a test that the
	// candidate made skip, or that stopped running at all. State written
	// before this field existed has none, and the baseline test step re-runs
	// to record them (CompletedSteps["baseline_test_passes"] marks the run).
	BaselineTestPasses []string `json:"baseline_test_passes,omitempty"`
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
	// Tradeoff overrides the manifest's performance block for this
	// campaign; the zero value leaves it as written.
	Tradeoff manifest.Tradeoff
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
	// tokenUsageBaseline anchors this process's contribution to
	// State.TokenUsage, the same way runStartedAt/elapsedBefore anchor its
	// contribution to State.ElapsedRunTime. See recordTokenUsage in adk.go.
	tokenUsageBaseline map[string]RoleUsageSnapshot
	// runStartedAt and elapsedBefore anchor this process's contribution to
	// State.ElapsedRunTime; runStartedAt stays zero until Run begins so that
	// engines driven straight through RunADK never advance the clock.
	runStartedAt  time.Time
	elapsedBefore time.Duration
	// pgoBuildTimeout overrides pgoLaneBuildTimeout. It is a field rather than
	// a constant so tests can drive the lane's bound without waiting minutes
	// for it, the same reason fence.go keeps its retry ladder in fields.
	pgoBuildTimeout time.Duration
}

func Create(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	repo, manifestPath, m, err := loadCampaignInputs(opts)
	if err != nil {
		return nil, err
	}
	if !opts.Tradeoff.IsZero() {
		if m.Performance, err = opts.Tradeoff.Apply(m.Performance); err != nil {
			return nil, err
		}
	}
	revision, goVersion, err := inspectRepoToolchain(ctx, toolchain.New(toolchain.Options{}), repo)
	if err != nil {
		return nil, err
	}
	now := opts.Now()
	id := stableID("campaign", revision, m.Name, now.Format(time.RFC3339Nano))
	dir, err := resolveCampaignDir(opts.CampaignDir, repo, id)
	if err != nil {
		return nil, err
	}
	return openCampaignEngine(opts, dir, id, repo, manifestPath, m, revision, goVersion, now)
}

func loadCampaignInputs(opts Options) (repo, manifestPath string, m manifest.Manifest, err error) {
	repo, err = filepath.Abs(opts.Repository)
	if err != nil {
		return "", "", manifest.Manifest{}, fmt.Errorf("resolve repository: %w", err)
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return "", "", manifest.Manifest{}, fmt.Errorf("resolve repository: %w", err)
	}
	manifestPath, err = filepath.Abs(opts.ManifestPath)
	if err != nil {
		return "", "", manifest.Manifest{}, fmt.Errorf("resolve manifest: %w", err)
	}
	m, err = manifest.LoadFile(manifestPath)
	if err != nil {
		return "", "", manifest.Manifest{}, err
	}
	return repo, manifestPath, m, nil
}

func inspectRepoToolchain(ctx context.Context, tc *toolchain.Toolchain, repo string) (revision, goVersion string, err error) {
	status, err := tc.GitStatus(ctx, repo)
	if err != nil {
		return "", "", fmt.Errorf("verify Git repository: %w", err)
	}
	if len(status.Stdout) != 0 {
		return "", "", fmt.Errorf("repository working tree must be clean (tracked and untracked files):\n%s", status.Stdout)
	}
	revisionResult, err := tc.GitRevision(ctx, repo)
	if err != nil {
		return "", "", fmt.Errorf("read source revision: %w", err)
	}
	goResult, err := tc.GoVersion(ctx, repo)
	if err != nil {
		return "", "", fmt.Errorf("verify local Go toolchain: %w", err)
	}
	return strings.TrimSpace(string(revisionResult.Stdout)), strings.TrimSpace(string(goResult.Stdout)), nil
}

func resolveCampaignDir(dir, repo, id string) (string, error) {
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache: %w", err)
		}
		dir = filepath.Join(cache, "gotorque", "campaigns", id)
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		dir = abs
	}
	if err := guardCampaignDir(dir, repo); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func guardCampaignDir(dir, repo string) error {
	if inside, relErr := filepath.Rel(repo, dir); relErr == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return errors.New("campaign directory must be outside the canonical repository")
	}
	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) != 0 {
		return fmt.Errorf("campaign directory %q is not empty; use --resume", dir)
	}
	return nil
}

func openCampaignEngine(opts Options, dir, id, repo, manifestPath string, m manifest.Manifest, revision, goVersion string, now time.Time) (*Engine, error) {
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	if err != nil {
		return nil, err
	}
	state := State{Version: 1, ID: id, Directory: dir, Repository: repo, ManifestPath: manifestPath, Manifest: m, Tradeoff: opts.Tradeoff,
		Status: StatusPending, StartedAt: now, UpdatedAt: now, CompletedSteps: map[string]bool{}, LocalIsolation: !opts.TestingUnsafeDisableIsolation,
		Environment: Environment{Authority: authority(), OS: runtime.GOOS, Architecture: runtime.GOARCH, CPU: cpuName(), GoVersion: goVersion, Revision: revision, BuildFlags: []string{"-mod=readonly", "-trimpath"}, CI: os.Getenv("CI") != "", CIEnvironment: ciEnvironment()},
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
	attachADK(e, opts)
	if err := e.saveEvent("campaign_created", "campaign initialized", nil); err != nil {
		_ = store.Close()
		return nil, err
	}
	return e, nil
}

func attachADK(e *Engine, opts Options) {
	e.adkAgents = opts.ADKAgents
	if opts.ADKConfig != nil {
		e.adkConfig = *opts.ADKConfig
	}
	if opts.ADKAgents != nil {
		e.state.ADKMode = "live"
	}
	e.noteAnalyst(opts.ADKAgents)
}

// noteAnalyst records a switch of the analyst, reviewer, or explorer to Jev. It never
// clears a mark: a campaign any part of which ran on Jev says so.
func (e *Engine) noteAnalyst(roleSet *agents.Set) {
	if roleSet == nil {
		return
	}
	if roleSet.CauseEvaluator != nil {
		e.state.Analyst = AnalystJev
	}
	if roleSet.ReviewEvaluator != nil {
		e.state.Reviewer = ReviewerJev
	}
	if roleSet.ExploreEvaluator != nil {
		e.state.Explorer = ExplorerJev
	}
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
	e.startRunClock()
	ctx, cancel := e.withCampaignDeadline(ctx)
	if cancel != nil {
		defer cancel()
	}
	e.state.Status, e.state.Error = StatusRunning, ""
	if err = e.saveEvent("campaign_started", "campaign running", nil); err != nil {
		return err
	}
	// A report exists from the start: the per-verdict snapshot leaves the first
	// ten minutes of a ninety-minute campaign unreadable, which is exactly the
	// window an operator wants to check that the run is on the right target.
	e.snapshotReports()
	defer e.captureRunFailure(ctx, &err)
	if err = e.runBaselineSteps(ctx); err != nil {
		return err
	}
	e.snapshotReports()
	if err = e.verifyClean(ctx); err != nil {
		return err
	}
	return e.finishCampaign(ctx)
}

// startRunClock anchors this process's slice of the campaign-wide elapsed
// time, which continues from the persisted total instead of from zero.
func (e *Engine) startRunClock() {
	e.runStartedAt, e.elapsedBefore = e.now(), e.state.ElapsedRunTime
}

// elapsedRunTime is the campaign-wide total as of now, including the slice of
// this process that no event has persisted yet. An engine that never started
// a run clock -- one driven straight through RunADK -- contributes nothing.
func (e *Engine) elapsedRunTime(now time.Time) time.Duration {
	if e.runStartedAt.IsZero() {
		return e.state.ElapsedRunTime
	}
	return e.elapsedBefore + now.Sub(e.runStartedAt)
}

// budgetPollInterval bounds how far past its wall-clock budget a campaign can
// run before guardWallClockBudget notices. It is deliberately short: the check
// is a single clock read against a fixed instant, and the failure it guards
// against is an overrun measured in hours.
const budgetPollInterval = 250 * time.Millisecond

// withCampaignDeadline spends what is left of the manifest's max_duration
// rather than granting it again. The bound names a campaign, not a process, so
// an interrupted campaign that resumed with a full budget could run for
// arbitrarily many multiples of max_duration across enough resumes.
func (e *Engine) withCampaignDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := e.state.Manifest.Campaign.MaxDuration.Duration()
	if budget <= 0 {
		return ctx, nil
	}
	// An exhausted budget yields an already-expired context, so a campaign
	// with nothing left stops down the same deadline path as one that runs
	// out mid-flight instead of needing a second terminal state.
	remaining := max(budget-e.state.ElapsedRunTime, 0)
	timed, cancelTimer := context.WithTimeoutCause(ctx, remaining, ErrDurationBudgetExhausted)
	guarded, cancelGuard := context.WithCancelCause(timed)
	// The guard needs the wall clock the campaign is actually charged against,
	// and only Run anchors it. An engine driven straight through RunADK never
	// advances that clock, so it has no wall-clock budget to police.
	if !e.runStartedAt.IsZero() {
		e.guardWallClockBudget(guarded, cancelGuard, e.runStartedAt.Add(remaining))
	}
	return guarded, func() {
		cancelGuard(context.Canceled)
		cancelTimer()
	}
}

// guardWallClockBudget stops the campaign once the wall clock says the budget
// is spent, which the context timer above cannot see on its own: Go timers run
// on the monotonic clock, and that clock stops while the machine is suspended.
// A laptop that sleeps two hours mid-campaign therefore hands a 90-minute
// context two extra hours of wall time, while ElapsedRunTime -- measured with
// time.Now, which keeps counting across a suspend -- charges every one of
// those minutes against the very same budget. Polling reconciles the two
// clocks so whichever runs out first ends the run.
//
// The goroutine owns no engine state beyond the clock and exits with ctx, so
// the cancel func returned by withCampaignDeadline also retires the guard.
func (e *Engine) guardWallClockBudget(ctx context.Context, cancel context.CancelCauseFunc, deadline time.Time) {
	go func() {
		ticker := time.NewTicker(budgetPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !e.now().Before(deadline) {
					cancel(ErrDurationBudgetExhausted)
					return
				}
			}
		}
	}()
}

func (e *Engine) captureRunFailure(ctx context.Context, err *error) {
	if *err == nil {
		return
	}
	e.state.UpdatedAt = e.now()
	// A spent budget surfaces as whatever call was in flight when the campaign
	// context died -- a git status, a model request, an ADK graph that drained
	// without producing a result -- which names an innocent bystander instead
	// of the bound that actually stopped the campaign. The context cause knows
	// better, so it wins.
	if errors.Is(context.Cause(ctx), ErrDurationBudgetExhausted) {
		budget := e.state.Manifest.Campaign.MaxDuration.Duration()
		e.state.StopReason = fmt.Sprintf("max_duration %s spent", budget)
		*err = fmt.Errorf("%w: spent %s of %s", ErrDurationBudgetExhausted, e.elapsedRunTime(e.state.UpdatedAt).Round(time.Second), budget)
	}
	e.state.Error = (*err).Error()
	if errors.Is(*err, ErrDurationBudgetExhausted) || errors.Is(*err, context.Canceled) || errors.Is(*err, context.DeadlineExceeded) {
		e.state.Status = StatusInterrupted
	} else {
		e.state.Status = StatusFailed
	}
	_ = e.saveEvent("campaign_stopped", e.state.Error, nil)
	// The terminal state has to reach the report files too. finishCampaign is
	// what normally writes them, and a campaign that stopped here never got
	// there, so the last thing on disk was a mid-run snapshot: gron-9 spent its
	// full ninety minutes and its report still said "running" with no stop
	// reason, while the database held `max_duration 1h30m0s spent`.
	e.snapshotReports()
}

func (e *Engine) runBaselineSteps(ctx context.Context) error {
	if err := e.runInspectStep(ctx); err != nil {
		return err
	}
	if err := e.runBuildStep(ctx); err != nil {
		return err
	}
	if err := e.runBaselineTestStep(ctx); err != nil {
		return err
	}
	if err := e.runSeedSteps(ctx); err != nil {
		return err
	}
	return e.runDiscoveryStep(ctx)
}

func (e *Engine) runInspectStep(ctx context.Context) error {
	if e.state.CompletedSteps["inspect"] {
		return nil
	}
	if err := e.inspect(ctx); err != nil {
		return err
	}
	e.state.CompletedSteps["inspect"] = true
	return e.saveEvent("repository_inspected", fmt.Sprintf("discovered %d packages", len(e.state.Inventory.Packages)), e.state.Inventory)
}

func (e *Engine) runBuildStep(ctx context.Context) error {
	if e.state.CompletedSteps["build"] {
		return nil
	}
	if err := e.build(ctx); err != nil {
		return err
	}
	e.state.CompletedSteps["build"] = true
	return e.saveEvent("baseline_built", "release-equivalent baseline built", map[string]string{"build_id": e.state.BuildID})
}

func (e *Engine) runSeedSteps(ctx context.Context) error {
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		step := "workload:" + seed.ID
		if e.state.CompletedSteps[step] {
			continue
		}
		if err := e.runSeed(ctx, seed); err != nil {
			return err
		}
		e.state.CompletedSteps[step] = true
		if err := e.saveEvent("workload_completed", seed.Name, map[string]string{"workload_id": seed.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) runDiscoveryStep(ctx context.Context) error {
	if e.state.CompletedSteps["discovery_profile"] {
		return nil
	}
	source := e.collectDiscoveryProfile(ctx)
	e.state.DiscoveryProfileSource = source
	e.state.CompletedSteps["discovery_profile"] = true
	return e.saveEvent("discovery_profile_completed", fmt.Sprintf("measured %d hot functions from %s", len(e.state.DiscoveryHotFunctions), source), nil)
}

func (e *Engine) finishCampaign(ctx context.Context) error {
	stopReason := "baseline discovery complete; no model candidate requested"
	if e.adkAgents != nil {
		result, err := e.RunADK(ctx, *e.adkAgents, e.adkConfig)
		if err != nil {
			return err
		}
		if result.ProviderFailure != "" {
			// Every model role failed in one cycle: no bound was reached, so
			// "completed" would misreport the campaign, and a completed campaign
			// cannot be resumed once the provider answers again. Returning the
			// failure lets captureRunFailure record it as failed with this stop
			// reason, and the command exits non-zero.
			e.state.StopReason = result.StopReason
			return fmt.Errorf("%w: %s", orchestrator.ErrProviderUnavailable, result.ProviderFailure)
		}
		if strings.TrimSpace(result.StopReason) != "" {
			stopReason = result.StopReason
		}
	}
	now := e.now()
	e.state.Status, e.state.CompletedAt, e.state.StopReason = StatusCompleted, &now, stopReason
	e.state.CompletedSteps["complete"] = true
	if err := e.saveEvent("campaign_completed", e.state.StopReason, nil); err != nil {
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
	workload := domain.Workload{ID: wid, Name: seed.Name, Seed: seed.ID, Tier: seed.Tier, Command: domain.Command{Path: e.state.DiscoveryBinaryPath, Args: append(append([]string{}, e.state.Manifest.Target.Command...), seed.Args...)}, Timeout: timeout, Provenance: seed.Provenance, Description: seed.Description}
	result, runErr := e.runner.Run(ctx, runner.RunRequest{Build: runner.Build{ID: e.state.DiscoveryBuildID, BinaryPath: e.state.DiscoveryBinaryPath}, Workload: workload, Mode: domain.RunModeDiscovery, NetworkAllowed: !e.state.LocalIsolation, FilesystemAllowed: !e.state.LocalIsolation, AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"}, Stdin: []byte(seed.Stdin), Fixtures: fixtures})
	result.ID = stableID("run", e.state.DiscoveryBuildID, wid, "baseline")
	e.state.Runs = append(e.state.Runs, result)
	if runErr != nil {
		return fmt.Errorf("baseline workload %q was invalid: %w", seed.ID, runErr)
	}
	return nil
}

const (
	profileSourceTargetSample = "a target sample"
	profileSourceBenchmark    = "target benchmarks"
	profileSourceNone         = "no source"
)

// collectDiscoveryProfile chooses where discovery hot paths come from, and
// returns that source's name for the completed event.
//
// The measured workloads come first, because they are what the campaign is
// about. A benchmark CPU profile weights every benchmark in the module equally
// regardless of how much it resembles the command, so microbenchmarks dominate
// the hot list and point the optimizer at code that cannot move the measured
// wall time: on gron three identifier microbenchmarks put validFirstRune at
// 36% cumulative while the measured workload's own hot frames (write,
// statements.Less, statement.String) never appeared, and the first two
// candidates of every campaign attacked rune classification before reaching
// the real cost. Sampling the release binary on a manifest seed workload
// profiles the execution the primary metric is taken from.
// Every path through this function records its own event, so it has no error
// to return: discovery evidence is best-effort and a missing source must not
// fail the campaign.
func (e *Engine) collectDiscoveryProfile(ctx context.Context) string {
	sampleErr := e.sampleTargetProfile(ctx)
	var source string
	switch sampleErr {
	case nil:
		// Still collect the benchmark profile when the module has benchmarks:
		// the informational PGO lane is built from it, and dropping that lane
		// because sampling won would be a silent feature regression.
		_, _ = e.benchmarkCPUProfile(ctx)
		source = profileSourceTargetSample
	default:
		if benchErr := e.profileHotFunctions(ctx); benchErr == nil {
			source = profileSourceBenchmark
		} else {
			_ = e.saveEvent("discovery_profile_skipped", fmt.Sprintf("direct target sampling unavailable (%s); benchmark CPU profile unavailable (%s)", sampleErr.Error(), benchErr.Error()), nil)
			return profileSourceNone
		}
	}
	return source + e.collectAllocationEvidence(ctx)
}

// collectAllocationEvidence adds a benchmark alloc_space profile to
// discovery's hot list when the campaign's objective is peak memory (ADR
// 0024), and returns a suffix naming that source for the discovery event, or
// "" when the objective is not memory or the module has no benchmarks.
func (e *Engine) collectAllocationEvidence(ctx context.Context) string {
	if e.state.Manifest.Performance.PrimaryMetric != objectivePeakMemory {
		return ""
	}
	if allocSource := e.profileAllocations(ctx); allocSource != "" {
		return " + " + allocSource
	}
	return ""
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
	seed, result, err := e.sampleFirstLiving(ctx)
	if err != nil {
		return err
	}
	results := append([]profile.SampleResult{result}, e.sampleExplored(ctx, seed)...)
	e.state.DiscoveryHotFunctions = e.resolveHotLocations(ctx, "", e.sampledHotNames(results...))
	e.state.DiscoveryProfileSummaryPath = result.RawReport
	return nil
}

// sampleFirstLiving samples the first seed, and when that fails, each
// stress-tier seed in manifest order, returning the first that sampled.
//
// Amplification only grows stdin, so a seed whose input is files runs as long
// as its files make it. go-jsonnet evaluates a .jsonnet file in 90 ms, and the
// macOS sampler cannot attach to a process that short: every attempt, even with
// sample -wait, wrote an empty call graph. Discovery then fell back to the
// module's benchmarks, which exercise unrelated code, and no target was chosen.
// A stress seed is the manifest's own larger version of a workload, so it can
// run long enough to sample.
func (e *Engine) sampleFirstLiving(ctx context.Context) (manifest.SeedWorkload, profile.SampleResult, error) {
	seeds := e.state.Manifest.Workloads.Seeds
	candidates := []manifest.SeedWorkload{seeds[0]}
	for _, seed := range seeds[1:] {
		if seed.Tier == domain.TierStress {
			candidates = append(candidates, seed)
		}
	}
	var failures []string
	for _, seed := range candidates {
		result, err := e.sampleSeed(ctx, seed, "sample-report.txt")
		if err == nil {
			return seed, result, nil
		}
		failures = append(failures, seed.ID+": "+err.Error())
	}
	return manifest.SeedWorkload{}, profile.SampleResult{}, errors.New(strings.Join(failures, "; "))
}

// sampleSeed samples one workload under the platform sampler, with its input
// amplified so the target outlives the sampling window.
func (e *Engine) sampleSeed(ctx context.Context, seed manifest.SeedWorkload, reportName string) (profile.SampleResult, error) {
	return e.sampleWith(ctx, seed, amplifyStdin([]byte(seed.Stdin)), reportName)
}

// sampleWith samples one workload on the given input.
func (e *Engine) sampleWith(ctx context.Context, seed manifest.SeedWorkload, stdin []byte, reportName string) (profile.SampleResult, error) {
	fixtures := make(map[string][]byte, len(seed.Files))
	for _, f := range seed.Files {
		fixtures[f.Path] = []byte(f.Content)
	}
	return profile.SampleTargetProfile(ctx, profile.SampleTarget{
		BinaryPath: e.state.BinaryPath,
		Args:       append(append([]string{}, e.state.Manifest.Target.Command...), seed.Args...),
		Stdin:      stdin,
		Fixtures:   fixtures,
		Duration:   4 * time.Second,
		OutputPath: filepath.Join(e.dir, "profile-sample", reportName),
	})
}

// sampledHotNames ranks the target's own functions by the samples spent on
// their behalf, then fills any budget left with the sampler's top-of-stack
// frames, which is all discovery used to list.
//
// Top of stack alone says which frames were executing, not for whom. A Go CLI
// that spends its time printing is executing fmt and write, so its hot list
// was standard-library names no patch can touch, and the function doing the
// printing had almost no self time: gron's per-statement Fprintln loop, whose
// bufio fix was the only patch ever accepted on it, never appeared, so no
// analyst was ever asked about it. Credited with the calls it makes, it ranks
// first.
//
// It returns up to twice the budget, because resolveHotLocations folds symbols
// that share a declaration and keeps resolving until the budget is filled.
//
// With explored workloads there are several samples; mergeAttributed weighs
// each as a whole, so a mode only a variant reaches ranks by its share of that
// variant's time rather than disappearing behind the seed.
func (e *Engine) sampledHotNames(results ...profile.SampleResult) []string {
	limit := 2 * hotFunctionBudget
	names := hotFunctionNames(mergeAttributed(results, e.ownSymbol), limit)
	for _, result := range results {
		for _, name := range hotFunctionNames(result.Functions, limit) {
			if len(names) < limit && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// mergeAttributed sums each sample's attributed weights as fractions of that
// sample's total, so every sampled workload counts equally whatever its length.
func mergeAttributed(results []profile.SampleResult, own func(string) bool) []profile.Function {
	weights := map[string]float64{}
	for _, result := range results {
		attributed := profile.AttributeToOwn(result.Stacks, own)
		total := 0.0
		for _, fn := range attributed {
			total += float64(atoiOrZero(fn.Flat))
		}
		if total == 0 {
			continue
		}
		for _, fn := range attributed {
			weights[fn.Name] += float64(atoiOrZero(fn.Flat)) / total
		}
	}
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if weights[names[i]] != weights[names[j]] {
			return weights[names[i]] > weights[names[j]]
		}
		return names[i] < names[j]
	})
	merged := make([]profile.Function, 0, len(names))
	for _, name := range names {
		merged = append(merged, profile.Function{Name: name, Flat: strconv.Itoa(int(weights[name] * 10000))})
	}
	return merged
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ownSymbol reports whether a sampled frame belongs to the target: its package
// is one of the module's, or main, which is how a command's own functions are
// named in its binary whatever its import path.
func (e *Engine) ownSymbol(symbol string) bool {
	pkg := profile.SymbolPackage(symbol)
	return pkg == "main" || (pkg != "" && slices.Contains(e.state.Inventory.Packages, pkg))
}

// amplifyStdin grows a seed input so a short-lived target stays alive for the
// sampler's window.
//
// Repeating the raw bytes only lengthens the run for a target that consumes
// all of stdin. A single-shot JSON CLI reads one document and ignores the
// rest: gron finished a 16 MiB concatenation of its 84 KiB seed in 21 ms,
// exactly as fast as the unamplified seed, so the target was gone before the
// sampler could attach and every campaign fell back to a benchmark profile.
// Replicating the elements of the document's largest array keeps the document
// valid and multiplies the work it describes, which turns that same seed into
// a multi-second run.
func amplifyStdin(stdin []byte) []byte {
	if len(stdin) == 0 || len(stdin) >= maxAmplifiedStdin {
		return stdin
	}
	if amplified, ok := amplifyJSONArray(stdin); ok {
		return amplified
	}
	return repeatStdin(stdin)
}

// maxAmplifiedStdin bounds a sampling input's size.
const maxAmplifiedStdin = 32 << 20

// amplificationTarget is how much input the amplifiers aim to produce.
const amplificationTarget = 16 << 20

// amplifyJSONArray duplicates the body of the largest JSON array so the result
// is still one valid document. It reports false when stdin is not JSON with a
// non-empty array, leaving the caller to fall back to byte repetition.
func amplifyJSONArray(stdin []byte) ([]byte, bool) {
	start, end, ok := largestJSONArray(stdin)
	if !ok {
		return nil, false
	}
	body := stdin[start+1 : end]
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	// The prefix already ends with '[', so each repetition contributes a
	// separating comma and one more element list.
	amplified := make([]byte, 0, amplificationTarget)
	amplified = append(amplified, stdin[:end]...)
	for len(amplified) < amplificationTarget {
		amplified = append(amplified, ',')
		amplified = append(amplified, body...)
	}
	return append(amplified, stdin[end:]...), true
}

// jsonSpan is a half-open byte range of a JSON container.
type jsonSpan struct{ start, end int }

// largestJSONArray returns the offsets of the '[' and ']' of the largest JSON
// array in data. The scan is string-aware so brackets inside string literals
// cannot confuse it, and it finds nested arrays because every closing bracket
// is compared against the opening one it matches.
func largestJSONArray(data []byte) (start, end int, ok bool) {
	var stack []int
	best := jsonSpan{start: -1, end: -1}
	var state jsonScanState
	for i := 0; i < len(data); i++ {
		if state.advance(data[i]) {
			continue
		}
		switch data[i] {
		case '[':
			stack = append(stack, i)
		case ']':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			best = widest(best, jsonSpan{start: open, end: i})
		}
	}
	if best.start < 0 {
		return 0, 0, false
	}
	return best.start, best.end, true
}

// jsonScanState tracks whether the scan is inside a string literal, so a
// bracket in string content is never mistaken for structure.
type jsonScanState struct {
	inString bool
	escaped  bool
}

// advance consumes one byte and reports whether it belonged to a string
// literal, meaning the caller must not treat it as structure.
func (s *jsonScanState) advance(c byte) bool {
	if !s.inString {
		if c == '"' {
			s.inString = true
			return true
		}
		return false
	}
	switch {
	case s.escaped:
		s.escaped = false
	case c == '\\':
		s.escaped = true
	case c == '"':
		s.inString = false
	}
	return true
}

func widest(a, b jsonSpan) jsonSpan {
	if b.end-b.start > a.end-a.start {
		return b
	}
	return a
}

// repeatStdin is the format-agnostic fallback: it lengthens the input for any
// target that consumes all of stdin.
// repeatLines repeats the input one copy per line, the shape a line-oriented
// mode reads as many documents.
func repeatLines(stdin []byte) []byte {
	return repeatStdin(append(bytes.TrimRight(stdin, "\n"), '\n'))
}

func repeatStdin(stdin []byte) []byte {
	amplified := make([]byte, 0, maxAmplifiedStdin)
	for len(amplified) < amplificationTarget {
		amplified = append(amplified, stdin...)
	}
	return amplified
}

// profileHotFunctions runs benchmarks under a CPU profile, then summarizes
// the top functions via go tool pprof.
//
// The target package is tried first, then the whole module. A CLI's command
// package usually holds no benchmarks while the library packages it drives do
// (gojq benchmarks its evaluator, not ./cmd/gojq), and a module-wide profile
// still carries exact file and line data for every sampled frame. Without the
// widened attempt those targets silently degrade to the OS sampler, whose
// frames carry no source position at all.
func (e *Engine) profileHotFunctions(ctx context.Context) error {
	cpuProfile, err := e.benchmarkCPUProfile(ctx)
	if err != nil {
		return err
	}
	return e.summarizeBenchmarkProfile(ctx, cpuProfile)
}

// benchmarkCPUProfile runs the module's benchmarks under -cpuprofile and
// records the profile as the PGO lane's input, returning its path. It is also
// called when the sampler already supplied the hot functions, because the
// informational PGO lane is built from this profile.
func (e *Engine) benchmarkCPUProfile(ctx context.Context) (string, error) {
	dir := filepath.Join(e.dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	cpuProfile := filepath.Join(dir, "bench-cpu.pb.gz")
	var benched bool
	for _, pkg := range benchmarkPackageOrder(e.state.Repository, e.state.Manifest.Target.Build.Package) {
		result, err := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: e.state.Repository, Packages: []string{pkg}, Bench: ".", Cpuprofile: cpuProfile, Output: filepath.Join(dir, "bench.test"), Env: []string{"GOTOOLCHAIN=local"}})
		if err != nil {
			continue
		}
		if strings.Contains(string(result.Stdout), "Benchmark") {
			benched = true
			break
		}
	}
	if !benched {
		return "", errors.New("no package in the module produced a benchmark CPU profile")
	}
	e.state.PGOProfilePath = cpuProfile
	return cpuProfile, nil
}

// summarizeBenchmarkProfile turns a benchmark CPU profile into hot functions
// annotated with repository-relative source positions.
func (e *Engine) summarizeBenchmarkProfile(ctx context.Context, cpuProfile string) error {
	artifacts, err := runner.NewArtifactStore(filepath.Join(e.dir, "artifacts"))
	if err != nil {
		return err
	}
	summary, err := profile.Collector{Toolchain: e.toolchain, Artifacts: artifacts}.SummarizePprof(ctx, cpuProfile, hotFunctionScanDepth)
	if err != nil {
		return fmt.Errorf("summarize benchmark CPU profile: %w", err)
	}
	e.state.DiscoveryHotFunctions = e.resolveHotLocations(ctx, cpuProfile, hotFunctionNames(summary.Functions, hotFunctionBudget))
	e.state.DiscoveryProfileSummaryPath = summary.RawReport
	return nil
}

// profileAllocations runs the module's benchmarks a second time under
// -memprofile and folds the alloc_space profile's hottest allocators to the
// front of discovery's hot list, ahead of any CPU-only function already
// there. It returns the source name for the discovery_profile_completed
// event, or "" when the module has no benchmarks — silent beyond the
// discovery_alloc_profile_skipped event, since discovery still has its CPU
// evidence to work from (ADR 0024).
//
// Only called under the peak_memory_bytes objective. The test binary this
// writes must stay out of the canonical checkout, so Output is set exactly
// as benchmarkCPUProfile sets it (see testArgs's -o handling in
// internal/toolchain/toolchain.go).
func (e *Engine) profileAllocations(ctx context.Context) string {
	dir := filepath.Join(e.dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	memProfile := filepath.Join(dir, "bench-mem.pb.gz")
	if !e.benchAllocations(ctx, dir, memProfile) {
		_ = e.saveEvent("discovery_alloc_profile_skipped", "no package in the module produced a benchmark allocation profile; hot list built from CPU evidence only", nil)
		return ""
	}
	locations, summaryPath, err := e.summarizeAllocProfile(ctx, memProfile)
	if err != nil {
		_ = e.saveEvent("discovery_alloc_profile_skipped", "summarize benchmark allocation profile: "+err.Error(), nil)
		return ""
	}
	e.state.DiscoveryHotFunctions = mergeAllocFirst(e.state.DiscoveryHotFunctions, locations, hotFunctionBudget)
	e.state.DiscoveryAllocProfileSummaryPath = summaryPath
	_ = e.saveEvent("discovery_alloc_profile_completed", fmt.Sprintf("measured %d allocation-heavy functions from the benchmark alloc_space profile", len(locations)), locations)
	return "a benchmark alloc_space profile"
}

// benchAllocations runs every candidate package's benchmarks under
// -memprofile, target package first, stopping at the first one that actually
// benchmarked something.
func (e *Engine) benchAllocations(ctx context.Context, dir, memProfile string) bool {
	for _, pkg := range benchmarkPackageOrder(e.state.Repository, e.state.Manifest.Target.Build.Package) {
		result, err := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: e.state.Repository, Packages: []string{pkg}, Bench: ".", Memprofile: memProfile, Output: filepath.Join(dir, "bench-mem.test"), Env: []string{"GOTOOLCHAIN=local"}})
		if err != nil {
			continue
		}
		if strings.Contains(string(result.Stdout), "Benchmark") {
			return true
		}
	}
	return false
}

// summarizeAllocProfile turns a benchmark heap profile into hot functions,
// ranked by the alloc_space sample index, with repository-relative source
// positions.
func (e *Engine) summarizeAllocProfile(ctx context.Context, memProfile string) ([]string, string, error) {
	artifacts, err := runner.NewArtifactStore(filepath.Join(e.dir, "artifacts"))
	if err != nil {
		return nil, "", err
	}
	summary, err := profile.Collector{Toolchain: e.toolchain, Artifacts: artifacts}.SummarizePprofAllocSpace(ctx, memProfile, hotFunctionScanDepth)
	if err != nil {
		return nil, "", err
	}
	locations := e.resolveHotLocations(ctx, memProfile, hotFunctionNames(summary.Functions, hotFunctionBudget))
	return locations, summary.RawReport, nil
}

// mergeAllocFirst puts every allocation-heavy location ahead of the
// CPU-derived hot list, preserving each list's own order, deduplicated and
// capped at budget: an allocator that never surfaced in the CPU profile still
// belongs in discovery's evidence, and one that did should not occupy two
// slots.
func mergeAllocFirst(cpu, alloc []string, budget int) []string {
	seen := make(map[string]bool, len(cpu)+len(alloc))
	merged := make([]string, 0, min(budget, len(cpu)+len(alloc)))
	for _, list := range [][]string{alloc, cpu} {
		for _, loc := range list {
			if seen[loc] || len(merged) == budget {
				continue
			}
			seen[loc] = true
			merged = append(merged, loc)
		}
	}
	return merged
}

// benchmarkPackageOrder lists the packages worth profiling, target package
// first so a target that benchmarks its own command keeps that evidence, then
// the module's benchmark-bearing packages richest first. Each is a single
// package because the go command rejects -cpuprofile for more than one.
func benchmarkPackageOrder(repository, targetPackage string) []string {
	order := []string{targetPackage}
	for _, pkg := range profile.BenchmarkPackages(repository) {
		if pkg != targetPackage {
			order = append(order, pkg)
		}
	}
	return order
}

// resolveHotLocations annotates hot function names with repository-relative
// source positions. Preferred source is `go tool pprof -list` over the
// benchmark profile (exact file and sampled line); the fallback searches the
// repository for the declaration. Unresolvable functions keep their bare
// names so downstream consumers never lose entries.
func (e *Engine) resolveHotLocations(ctx context.Context, cpuProfile string, names []string) []string {
	locations := make([]string, 0, hotFunctionBudget)
	for _, name := range names {
		if len(locations) == hotFunctionBudget {
			break
		}
		// A value method and the pointer wrapper Go generates for it are two
		// symbols with one declaration: gron's hot list named statements.go:312
		// twice, spending a slot of the budget on a repeat.
		if loc := e.hotLocation(ctx, cpuProfile, name); !slices.Contains(locations, loc) {
			locations = append(locations, loc)
		}
	}
	return locations
}

func (e *Engine) hotLocation(ctx context.Context, cpuProfile, name string) string {
	if loc, ok := e.hotLocationFromProfile(ctx, name, cpuProfile); ok {
		return loc
	}
	if loc, ok := e.hotLocationFromRepo(name); ok {
		return loc
	}
	return name
}

func (e *Engine) hotLocationFromProfile(ctx context.Context, name, cpuProfile string) (string, bool) {
	if cpuProfile == "" {
		return "", false
	}
	result, err := e.toolchain.PprofList(ctx, name, cpuProfile)
	if err != nil {
		return "", false
	}
	path, line, ok := profile.ParsePprofList(string(result.Stdout))
	if !ok {
		return "", false
	}
	path, ok = e.repoRelative(path)
	if !ok {
		return "", false
	}
	return profile.HotLocation{Function: name, Path: path, Line: line}.Location(), true
}

// repoRelative rewrites a profiler's absolute source path into the
// repository-relative form the excerpt collector requires, and rejects paths
// outside the repository.
//
// `go tool pprof -list` reports absolute paths. extractExcerpts refuses those
// because an absolute location is indistinguishable from one escaping the
// repository, so every profiled frame resolved to a location no source window
// could ever be read from: a target checked out under a path the profiler
// echoed back produced one usable excerpt out of eleven measured functions.
// Frames in the standard library or module cache are dropped outright rather
// than kept as bare paths, since no patch this campaign may write can reach
// them and they otherwise occupy the excerpt budget.
func (e *Engine) repoRelative(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path), true
	}
	// Both sides are compared in raw and symlink-resolved form. macOS resolves
	// a temporary root through /private while a source file the profiler named
	// may not resolve at all, and comparing one resolved path against one raw
	// path reports a file inside the repository as escaping it.
	for _, root := range symlinkForms(e.state.Repository) {
		for _, candidate := range symlinkForms(path) {
			rel, err := filepath.Rel(root, candidate)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// symlinkForms returns the path as given and, when it differs, its
// symlink-resolved form.
func symlinkForms(path string) []string {
	forms := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		forms = append(forms, resolved)
	}
	return forms
}

func (e *Engine) hotLocationFromRepo(name string) (string, bool) {
	if strings.Contains(name, ":") {
		return "", false
	}
	// Sampler output uses runtime-style names (main.main,
	// pkg.(*T).method) whose last segment is the source symbol.
	for _, candidate := range functionNameCandidates(name) {
		if path, line, ok := profile.FindFunctionInRepo(e.state.Repository, candidate); ok {
			return profile.HotLocation{Function: name, Path: path, Line: line}.Location(), true
		}
	}
	return "", false
}

// functionNameCandidates lists the identifiers worth searching for in the
// repository, most specific first. Profile frames name closures and methods in
// forms no `func` declaration ever uses: pkg.outer.func1 for a closure (and
// .func1.2 when nested), pkg.(*T).method for a method. Searching those
// verbatim never matches, which is why a measured module symbol such as
// cli.newJSONInputIter.func1 used to resolve to no source location at all.
func functionNameCandidates(name string) []string {
	candidates := []string{name}
	add := func(candidate string) {
		if candidate == "" {
			return
		}
		for _, existing := range candidates {
			if existing == candidate {
				return
			}
		}
		candidates = append(candidates, candidate)
	}

	// Strip closure suffixes until an enclosing declaration name remains.
	enclosing := name
	for {
		trimmed, ok := trimClosureSuffix(enclosing)
		if !ok {
			break
		}
		enclosing = trimmed
		add(enclosing)
	}

	// The declared identifier is the final segment, with any method receiver
	// removed: pkg.(*T).method declares "func (t *T) method(".
	add(lastSegment(enclosing))
	add(lastSegment(name))
	return candidates
}

func isTestEntryPoint(segment string) bool {
	for _, prefix := range []string{"Benchmark", "Test", "Fuzz", "Example"} {
		if strings.HasPrefix(segment, prefix) {
			return true
		}
	}
	return false
}

var closureSuffix = regexp.MustCompile(`\.func\d+$`)

func trimClosureSuffix(name string) (string, bool) {
	if loc := closureSuffix.FindStringIndex(name); loc != nil {
		return name[:loc[0]], true
	}
	// Nested closures append an ordinal: outer.func1.2.
	if idx := strings.LastIndex(name, "."); idx > 0 {
		if _, err := strconv.Atoi(name[idx+1:]); err == nil {
			return name[:idx], true
		}
	}
	return name, false
}

func lastSegment(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 && idx < len(name)-1 {
		return name[idx+1:]
	}
	return name
}

const (
	// hotFunctionBudget caps how many actionable functions reach the agents.
	hotFunctionBudget = 15
	// hotFunctionScanDepth is how many profile nodes are summarized to fill
	// that budget. A Go CPU profile's hottest nodes are overwhelmingly
	// runtime scheduler and allocator frames, so scanning only as deep as the
	// budget yields a handful of module functions and wastes the rest of the
	// budget on frames no patch can touch.
	hotFunctionScanDepth = 4 * hotFunctionBudget
)

// hotFunctionNames extracts deduplicated function names from a parsed pprof
// top summary, skipping runtime frames that never belong to the target.
func hotFunctionNames(functions []profile.Function, limit int) []string {
	names := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, fn := range functions {
		name := strings.TrimSpace(fn.Name)
		if name == "" || strings.HasPrefix(name, "runtime.") || seen[name] || !actionableSymbol(name) {
			continue
		}
		seen[name] = true
		names = append(names, name)
		if len(names) == limit {
			break
		}
	}
	return names
}

// actionableSymbol rejects frames no source change can address.
//
// The OS sampler reports kernel and libc symbols (__psynch_cvwait, kevent,
// nanosleep) that describe a process waiting, not computing. Go symbols always
// carry a package qualifier, so the absence of a dot is a reliable
// discriminator for those.
//
// Benchmark CPU profiles additionally carry the harness that drove them
// (testing.(*B).runN and friends). Those frames are an artifact of how the
// measurement was taken rather than of the program under test, and agents
// otherwise rank them as top hot paths and reason about them as target code.
func actionableSymbol(name string) bool {
	if strings.HasPrefix(name, "_") {
		return false
	}
	if strings.HasPrefix(name, "testing.") {
		return false
	}
	// The module's own benchmark, test and fuzz entry points are sampled too
	// when the profile comes from its test binary. They are measurement
	// scaffolding, not the program, and a patch to one optimizes nothing the
	// target ships.
	if isTestEntryPoint(lastSegment(name)) {
		return false
	}
	return strings.Contains(name, ".")
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
	info, err := os.Stat(vendor)
	if err == nil && info.IsDir() {
		paths, err = walkVendorFiles(repository, paths)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return digestFiles(repository, paths)
}

func walkVendorFiles(repository string, paths []string) ([]string, error) {
	err := filepath.WalkDir(filepath.Join(repository, "vendor"), func(path string, entry os.DirEntry, walkErr error) error {
		return appendVendorFile(repository, path, entry, walkErr, &paths)
	})
	return paths, err
}

func appendVendorFile(repository, path string, entry os.DirEntry, walkErr error, paths *[]string) error {
	if walkErr != nil {
		return walkErr
	}
	if !entry.Type().IsRegular() {
		return nil
	}
	rel, err := filepath.Rel(repository, path)
	if err != nil {
		return err
	}
	*paths = append(*paths, rel)
	return nil
}

func digestFiles(repository string, paths []string) (map[string]string, error) {
	result := make(map[string]string, len(paths))
	for _, rel := range paths {
		digest, err := runner.DigestFile(filepath.Join(repository, rel))
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
	// Advancing the campaign clock on every persisted event bounds what an
	// abrupt kill can hand back: at most the work since the previous event,
	// rather than everything this process had already spent.
	e.state.ElapsedRunTime = e.elapsedRunTime(now)
	if e.adkAgents != nil {
		// Refresh the persisted token spend on every event while an ADK run is
		// active, not only when RunADK returns via its deferred call: a
		// SIGKILLed process never runs that defer, and without this a killed
		// campaign's spend never reaches bbolt at all.
		e.recordTokenUsage(*e.adkAgents)
	}
	if err := e.store.Save(e.state); err != nil {
		return err
	}
	if err := e.store.Append(Event{Time: now, Type: kind, Message: message, Data: data}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.progress, "[%s] %s\n", kind, message)
	return nil
}

func (e *Engine) snapshotReports() {
	// Snapshot failure is not fatal: the verdicts live in bbolt and the report
	// is rewritten when the campaign completes, but a silent failure would
	// leave an operator reading a stale snapshot with no hint it is stale.
	if err := WriteReports(e.dir, e.state); err != nil {
		_, _ = fmt.Fprintf(e.progress, "[report_snapshot_failed] %v\n", err)
	}
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
	// Read-only file; a Close error here carries no data-loss meaning.
	defer func() { _ = file.Close() }()
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
	e.noteAnalyst(roleSet)
}
