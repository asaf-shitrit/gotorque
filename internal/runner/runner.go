package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/toolchain"
)

// Runner only launches a configured build artifact. It never accepts a shell
// expression and rejects workload paths that differ from that artifact.
type Runner struct {
	executor        toolchain.Executor
	artifacts       *ArtifactStore
	sandboxRoot     string
	networkGuard    NetworkGuard
	filesystemGuard FilesystemGuard
	keepFailedRuns  bool
	localIsolation  bool
	sandbox         SandboxPolicy
	now             func() time.Time
}

type Options struct {
	Executor        toolchain.Executor
	Artifacts       *ArtifactStore
	SandboxRoot     string
	NetworkGuard    NetworkGuard
	FilesystemGuard FilesystemGuard
	KeepFailedRuns  bool
	LocalIsolation  bool
	// Sandbox is the target manifest's sandbox block, translated into the
	// primitives this runner enforces. It applies to every run this Runner
	// makes -- one manifest per campaign -- so baseline and candidate runs
	// share identical policy. The zero value denies network and restricts
	// writes to the sandbox directory, matching the manifest loader's
	// defaults.
	Sandbox SandboxPolicy
}

func New(opts Options) (*Runner, error) {
	if opts.Executor == nil {
		opts.Executor = toolchain.OSExecutor{}
	}
	if opts.Artifacts == nil {
		return nil, errors.New("artifact store is required")
	}
	if !filepath.IsAbs(opts.SandboxRoot) {
		return nil, errors.New("sandbox root must be absolute")
	}
	return &Runner{executor: opts.Executor, artifacts: opts.Artifacts, sandboxRoot: opts.SandboxRoot, networkGuard: opts.NetworkGuard, filesystemGuard: opts.FilesystemGuard, keepFailedRuns: opts.KeepFailedRuns, localIsolation: opts.LocalIsolation, sandbox: opts.Sandbox, now: func() time.Time { return time.Now().UTC() }}, nil
}

type Build struct {
	ID         string
	BinaryPath string
}

type RunRequest struct {
	Build         Build
	Workload      domain.Workload
	Mode          domain.RunMode
	AdditionalEnv map[string]string
	Stdin         []byte
	Fixtures      map[string][]byte
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (domain.RunResult, error) {
	if err := validateRunRequest(req); err != nil {
		return domain.RunResult{}, err
	}
	sandbox, plan, err := r.newRunSandbox()
	if err != nil {
		return domain.RunResult{}, err
	}
	success := false
	defer func() { _ = sandbox.Close(success) }()
	if err := materializeFixtures(sandbox.WorkDir, req.Fixtures); err != nil {
		return domain.RunResult{}, err
	}
	stdinReader, closer, err := openStdin(req)
	if err != nil {
		return domain.RunResult{}, err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	env, err := discoveryEnv(sandbox, r.sandbox.Environment, req)
	if err != nil {
		return domain.RunResult{}, err
	}
	workloadCtx, cancel := withWorkloadTimeout(ctx, req.Workload.Timeout)
	defer cancel()
	started := r.now()
	commandPath, commandArgs, isolationNotes, err := r.prepareCommand(ctx, sandbox, plan, req)
	if err != nil {
		return domain.RunResult{}, err
	}
	commandResult, runErr := r.executor.Run(workloadCtx, toolchain.Invocation{
		Path: commandPath, Args: commandArgs,
		Dir: sandbox.WorkDir, Env: env, Stdin: stdinReader,
	})
	result := buildRunResult(req, started, commandResult, runErr)
	result.IsolationNotes = isolationNotes
	if err := r.collectRunArtifacts(&result, sandbox, commandResult, req.Mode); err != nil {
		return result, err
	}
	success = runErr == nil
	return result, runErr
}

// runPlan is what the Runner's sandbox policy resolves to for one run:
// whether network and filesystem access are granted, and whether local
// isolation (sandbox-exec/bwrap exec wrapping) applies. It exists so
// newRunSandbox and prepareCommand -- which need the same three booleans --
// do not each recompute them, which was the source of Run's complexity.
type runPlan struct {
	networkDisabled     bool
	filesystemAllowed   bool
	usingLocalIsolation bool
}

// newRunSandbox resolves the Runner's sandbox policy (the manifest) into a
// runPlan and builds the sandbox directory for one run. Policy comes from
// the Runner, not the request: every run this Runner makes shares one
// policy, so a baseline and a candidate are never isolated differently.
// r.localIsolation false means no enforcement mechanism is available at all
// (tests, or TestingUnsafeDisableIsolation); in that mode both network and
// filesystem are granted because there is no guard to enforce a denial.
func (r *Runner) newRunSandbox() (*Sandbox, runPlan, error) {
	networkAllowed := !r.localIsolation || r.sandbox.NetworkAllowed
	filesystemAllowed := !r.localIsolation || r.sandbox.filesystemUnrestricted()
	plan := runPlan{networkDisabled: !networkAllowed, filesystemAllowed: filesystemAllowed}
	plan.usingLocalIsolation = r.useLocalIsolation(plan.networkDisabled, filesystemAllowed)
	sandbox, err := NewSandbox(r.sandboxOptions(filesystemAllowed, plan.networkDisabled, plan.usingLocalIsolation))
	return sandbox, plan, err
}

// prepareCommand resolves the command a run plan and request produce: the
// local-isolation wrapping (if any), then rlimit wrapping, plus every
// isolation note either step recorded.
func (r *Runner) prepareCommand(ctx context.Context, sandbox *Sandbox, plan runPlan, req RunRequest) (string, []string, []string, error) {
	commandPath, commandArgs, err := maybeIsolate(ctx, plan.usingLocalIsolation, sandbox, plan.networkDisabled, req)
	if err != nil {
		return "", nil, nil, err
	}
	notes := r.isolationNotes(ctx, plan.usingLocalIsolation, plan.networkDisabled, !plan.filesystemAllowed)
	commandPath, commandArgs, limitNotes := WrapWithResourceLimits(ctx, r.sandbox.Limits, commandPath, commandArgs)
	return commandPath, commandArgs, append(notes, limitNotes...), nil
}

// isolationNotes surfaces anything the sandbox policy asked for that this
// platform or environment could not fully honor: probe-based local
// isolation degradation plus the filesystem scopes gotorque cannot yet
// narrow to a manifest path list. It runs after maybeIsolate so the
// bubblewrap capability probes it reads are already warm.
func (r *Runner) isolationNotes(ctx context.Context, localIsolation, networkDisabled, filesystemRestricted bool) []string {
	var notes []string
	if localIsolation {
		notes = append(notes, localIsolationNotes(ctx, runtime.GOOS, networkDisabled, filesystemRestricted)...)
	}
	if filesystemRestricted {
		notes = append(notes, writeScopeNotes(r.sandbox)...)
	}
	notes = append(notes, readScopeNotes(r.sandbox)...)
	return notes
}

func (r *Runner) useLocalIsolation(networkDisabled, filesystemAllowed bool) bool {
	return r.localIsolation && (networkDisabled || !filesystemAllowed)
}

func (r *Runner) sandboxOptions(filesystemAllowed, networkDisabled, localIsolation bool) SandboxOptions {
	return SandboxOptions{
		Root: r.sandboxRoot, NetworkDisabled: networkDisabled && !localIsolation, NetworkGuard: r.networkGuard,
		FilesystemRestricted: !filesystemAllowed && !localIsolation, FilesystemGuard: r.filesystemGuard,
		KeepOnFailure: r.keepFailedRuns,
	}
}

func openStdin(req RunRequest) (io.Reader, io.Closer, error) {
	var file *os.File
	if req.Workload.StdinPath != "" {
		var err error
		file, err = openInput(req.Workload.StdinPath)
		if err != nil {
			return nil, nil, err
		}
	}
	// A typed-nil *os.File must never reach exec.Cmd: os/exec would wire the
	// child's descriptor 0 to an invalid file and Go runtimes abort on startup
	// when standard descriptors are closed.
	if len(req.Stdin) > 0 {
		return bytes.NewReader(req.Stdin), file, nil
	}
	if file != nil {
		return file, file, nil
	}
	return nil, nil, nil
}

func discoveryEnv(sandbox *Sandbox, policy EnvironmentPolicy, req RunRequest) ([]string, error) {
	env := BuildEnv(policy, sandbox.Env(), req.AdditionalEnv)
	if req.Mode != domain.RunModeDiscovery {
		return env, nil
	}
	coverageDir := filepath.Join(sandbox.Root, "coverage")
	if err := os.MkdirAll(coverageDir, 0o700); err != nil {
		return nil, err
	}
	return append(env, "GOCOVERDIR="+coverageDir), nil
}

func withWorkloadTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

func maybeIsolate(ctx context.Context, localIsolation bool, sandbox *Sandbox, networkDisabled bool, req RunRequest) (string, []string, error) {
	path := req.Build.BinaryPath
	args := append([]string(nil), req.Workload.Command.Args...)
	if !localIsolation {
		return path, args, nil
	}
	return isolatedCommand(ctx, sandbox.Root, sandbox.WorkDir, networkDisabled, path, args)
}

func buildRunResult(req RunRequest, started time.Time, commandResult toolchain.Result, runErr error) domain.RunResult {
	result := domain.RunResult{
		ID: runID(req.Build.ID, req.Workload.ID, started), BuildID: req.Build.ID, WorkloadID: req.Workload.ID, Workload: req.Workload.Seed,
		Mode: req.Mode, StartedAt: started, Duration: commandResult.Duration, ExitCode: commandResult.ExitCode,
		StdoutDigest: Digest(commandResult.Stdout), StderrDigest: Digest(commandResult.Stderr),
		// Order-insensitive digest: identical multiset of output lines
		// yields identical values even when tie ordering varies between
		// runs. Used only when the baseline proves itself nondeterministic.
		SortedLinesDigest: Digest(sortedLines(commandResult.Stdout)),
		Metrics: []domain.Metric{
			{Name: "wall_time_ns", Unit: "ns", Value: float64(commandResult.Duration)},
			{Name: "cpu_time_ns", Unit: "ns", Value: float64(commandResult.UserCPU + commandResult.SystemCPU)},
			{Name: "cpu_user_ns", Unit: "ns", Value: float64(commandResult.UserCPU)},
			{Name: "cpu_system_ns", Unit: "ns", Value: float64(commandResult.SystemCPU)},
			{Name: "peak_memory_bytes", Unit: "bytes", Value: float64(commandResult.MaxRSSBytes)},
		}, Artifacts: map[string]string{},
	}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	return result
}

func (r *Runner) collectRunArtifacts(result *domain.RunResult, sandbox *Sandbox, commandResult toolchain.Result, mode domain.RunMode) error {
	if err := r.putRunOutput(result, commandResult); err != nil {
		return err
	}
	files, err := r.artifacts.SnapshotFiles(sandbox.WorkDir)
	if err != nil {
		return err
	}
	for key, value := range files {
		result.Artifacts[key] = value
	}
	if mode != domain.RunModeDiscovery {
		return nil
	}
	return r.snapshotCoverage(result, sandbox.Root)
}

func (r *Runner) putRunOutput(result *domain.RunResult, commandResult toolchain.Result) error {
	if err := r.putArtifact(result, "stdout", commandResult.Stdout); err != nil {
		return err
	}
	return r.putArtifact(result, "stderr", commandResult.Stderr)
}

func (r *Runner) putArtifact(result *domain.RunResult, name string, data []byte) error {
	_, path, err := r.artifacts.Put(name, data)
	if err != nil {
		return err
	}
	result.Artifacts[name] = path
	return nil
}

func (r *Runner) snapshotCoverage(result *domain.RunResult, root string) error {
	coverage, err := r.artifacts.SnapshotFiles(filepath.Join(root, "coverage"))
	if err != nil {
		return err
	}
	for key, value := range coverage {
		result.Artifacts["coverage:"+key] = value
	}
	return nil
}

func isolatedCommand(ctx context.Context, root, workDir string, denyNetwork bool, path string, args []string) (string, []string, error) {
	return isolatedCommandFor(ctx, runtime.GOOS, root, workDir, denyNetwork, path, args)
}

// isolatedCommandFor takes the platform as an argument so the wrapper decision
// logic can be tested on any host without a sandbox being installed there.
func isolatedCommandFor(ctx context.Context, goos, root, workDir string, denyNetwork bool, path string, args []string) (string, []string, error) {
	switch goos {
	case "darwin":
		profile := "(version 1)(allow default)(deny file-write*)(allow file-write* (subpath \"" + strings.ReplaceAll(root, "\"", "\\\"") + "\"))"
		if denyNetwork {
			profile += "(deny network*)"
		}
		return "/usr/bin/sandbox-exec", append([]string{"-p", profile, path}, args...), nil
	case "linux":
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			return "", nil, errors.New("local Linux isolation requires bubblewrap (bwrap)")
		}
		// Some environments (nested CI containers) block the mount and
		// namespace operations bubblewrap needs. Probe once; if isolation
		// cannot be established here, run unwrapped rather than failing
		// every campaign. Authoritative measurement environments should
		// provide working bubblewrap.
		if !bwrapIsolationSupported(ctx, bwrap, root, workDir) {
			return path, args, nil
		}
		wrapped := []string{"--die-with-parent"}
		if denyNetwork && bwrapNetNamespaceSupported(ctx, bwrap) {
			wrapped = append(wrapped, "--unshare-net")
		}
		wrapped = append(wrapped, "--ro-bind", "/", "/", "--bind", root, root, "--chdir", workDir, path)
		return bwrap, append(wrapped, args...), nil
	default:
		return "", nil, fmt.Errorf("local isolation is unsupported on %s", goos)
	}
}

// bwrapProbe caches whether this environment can run bubblewrap at all:
// both a trivial namespace setup and the bind/chdir shape isolatedCommand
// uses. Nested CI containers frequently block the mount calls involved.
var (
	bwrapProbeOnce sync.Once
	bwrapProbeOK   bool
)

func bwrapIsolationSupported(ctx context.Context, bwrap, root, workDir string) bool {
	bwrapProbeOnce.Do(func() {
		probe := exec.CommandContext(ctx, bwrap, "--die-with-parent", "--ro-bind", "/", "/",
			"--bind", root, root, "--chdir", workDir, "/bin/true")
		bwrapProbeOK = probe.Run() == nil
	})
	return bwrapProbeOK
}

// bwrapNetNamespaceSupported probes whether bubblewrap can create a network
// namespace. Some sandboxed CI environments block the loopback configuration
// bwrap performs even with elevated capabilities; there we degrade to
// filesystem-only isolation instead of failing every run.
var (
	bwrapNetNamespaceOnce sync.Once
	bwrapNetNamespaceOK   bool
)

func bwrapNetNamespaceSupported(ctx context.Context, bwrap string) bool {
	bwrapNetNamespaceOnce.Do(func() {
		probe := exec.CommandContext(ctx, bwrap, "--unshare-net", "--ro-bind", "/", "/", "--dev-bind", "/dev", "/dev", "true")
		bwrapNetNamespaceOK = probe.Run() == nil
	})
	return bwrapNetNamespaceOK
}

func materializeFixtures(root string, fixtures map[string][]byte) error {
	for name, content := range fixtures {
		if name == "" || filepath.IsAbs(name) {
			return fmt.Errorf("fixture path %q must be relative", name)
		}
		clean := filepath.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("fixture path %q escapes the sandbox", name)
		}
		path := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func validateRunRequest(req RunRequest) error {
	if err := validateBuild(req.Build); err != nil {
		return err
	}
	if req.Workload.ID == "" {
		return errors.New("workload ID is required")
	}
	if err := validateRunMode(req.Mode); err != nil {
		return err
	}
	return validateWorkloadBinary(req)
}

func validateBuild(b Build) error {
	if b.ID == "" || b.BinaryPath == "" || !filepath.IsAbs(b.BinaryPath) {
		return errors.New("build ID and absolute binary path are required")
	}
	_, err := os.Stat(b.BinaryPath)
	return err
}

func validateRunMode(mode domain.RunMode) error {
	switch mode {
	case domain.RunModeDiscovery, domain.RunModeDiagnosis, domain.RunModeMeasurement, domain.RunModeValidation:
		return nil
	default:
		return fmt.Errorf("unsupported run mode %q", mode)
	}
}

func validateWorkloadBinary(req RunRequest) error {
	if req.Workload.Command.Path == "" {
		return nil
	}
	a, errA := filepath.EvalSymlinks(req.Build.BinaryPath)
	b, errB := filepath.EvalSymlinks(req.Workload.Command.Path)
	if errA != nil || errB != nil || a != b {
		return errors.New("workload command path must be the configured build binary")
	}
	return nil
}

func openInput(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("stdin path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errors.New("stdin path resolved to a nil file")
	}
	return file, nil
}

func mapEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" || filepath.Base(key) != key {
			continue
		}
		result = append(result, key+"="+values[key])
	}
	return result
}

func runID(buildID, workloadID string, started time.Time) string {
	return Digest([]byte(buildID + "\x00" + workloadID + "\x00" + started.Format(time.RFC3339Nano)))[:24]
}

type ABRequest struct {
	Baseline    RunRequest
	Candidate   RunRequest
	Repetitions int
}

type ABResult struct {
	Baseline  []domain.RunResult
	Candidate []domain.RunResult
}

// RunInterleaved serializes A/B measurements to prevent CPU contention. The
// order is baseline,candidate for every pair, giving an A/B/A/B sequence.
func (r *Runner) RunInterleaved(ctx context.Context, req ABRequest) (ABResult, error) {
	if req.Repetitions <= 0 {
		return ABResult{}, errors.New("repetitions must be positive")
	}
	if req.Baseline.Mode != domain.RunModeMeasurement || req.Candidate.Mode != domain.RunModeMeasurement {
		return ABResult{}, errors.New("interleaved runs require measurement mode")
	}
	if req.Baseline.Workload.ID != req.Candidate.Workload.ID {
		return ABResult{}, errors.New("baseline and candidate must use the same workload")
	}
	result := ABResult{Baseline: make([]domain.RunResult, 0, req.Repetitions), Candidate: make([]domain.RunResult, 0, req.Repetitions)}
	for i := 0; i < req.Repetitions; i++ {
		baseline, err := r.Run(ctx, req.Baseline)
		result.Baseline = append(result.Baseline, baseline)
		if err != nil {
			return result, err
		}
		candidate, err := r.Run(ctx, req.Candidate)
		result.Candidate = append(result.Candidate, candidate)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func sortedLines(stdout []byte) []byte {
	lines := strings.Split(string(stdout), "\n")
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n"))
}
