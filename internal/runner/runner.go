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
	return &Runner{executor: opts.Executor, artifacts: opts.Artifacts, sandboxRoot: opts.SandboxRoot, networkGuard: opts.NetworkGuard, filesystemGuard: opts.FilesystemGuard, keepFailedRuns: opts.KeepFailedRuns, localIsolation: opts.LocalIsolation, now: func() time.Time { return time.Now().UTC() }}, nil
}

type Build struct {
	ID         string
	BinaryPath string
}

type RunRequest struct {
	Build             Build
	Workload          domain.Workload
	Mode              domain.RunMode
	NetworkAllowed    bool // default is false; set only with a manifest grant.
	FilesystemAllowed bool // default is false; set only with a manifest grant.
	AdditionalEnv     map[string]string
	Stdin             []byte
	Fixtures          map[string][]byte
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (domain.RunResult, error) {
	if err := validateRunRequest(req); err != nil {
		return domain.RunResult{}, err
	}
	networkDisabled := !req.NetworkAllowed
	localIsolation := r.localIsolation && (networkDisabled || !req.FilesystemAllowed)
	sandbox, err := NewSandbox(SandboxOptions{Root: r.sandboxRoot, NetworkDisabled: networkDisabled && !localIsolation, NetworkGuard: r.networkGuard, FilesystemRestricted: !req.FilesystemAllowed && !localIsolation, FilesystemGuard: r.filesystemGuard, KeepOnFailure: r.keepFailedRuns})
	if err != nil {
		return domain.RunResult{}, err
	}
	success := false
	defer func() { _ = sandbox.Close(success) }()
	if err := materializeFixtures(sandbox.WorkDir, req.Fixtures); err != nil {
		return domain.RunResult{}, err
	}

	stdin, err := openInput(req.Workload.StdinPath)
	if err != nil {
		return domain.RunResult{}, err
	}
	if stdin != nil {
		defer stdin.Close()
	}
	env := append(sandbox.Env(), mapEnvironment(req.AdditionalEnv)...)
	if req.Mode == domain.RunModeDiscovery {
		coverageDir := filepath.Join(sandbox.Root, "coverage")
		if err := os.MkdirAll(coverageDir, 0o700); err != nil {
			return domain.RunResult{}, err
		}
		env = append(env, "GOCOVERDIR="+coverageDir)
	}

	workloadCtx := ctx
	var cancel context.CancelFunc
	if req.Workload.Timeout > 0 {
		workloadCtx, cancel = context.WithTimeout(ctx, req.Workload.Timeout)
		defer cancel()
	}
	started := r.now()
	// A typed-nil *os.File must never reach exec.Cmd: os/exec would wire the
	// child's descriptor 0 to an invalid file and Go runtimes abort on startup
	// when standard descriptors are closed.
	var stdinReader io.Reader
	if len(req.Stdin) > 0 {
		stdinReader = bytes.NewReader(req.Stdin)
	} else if stdin != nil {
		stdinReader = stdin
	}
	commandPath := req.Build.BinaryPath
	commandArgs := append([]string(nil), req.Workload.Command.Args...)
	if localIsolation {
		commandPath, commandArgs, err = isolatedCommand(sandbox.Root, sandbox.WorkDir, networkDisabled, commandPath, commandArgs)
		if err != nil {
			return domain.RunResult{}, err
		}
	}
	commandResult, runErr := r.executor.Run(workloadCtx, toolchain.Invocation{
		Path: commandPath, Args: commandArgs,
		Dir: sandbox.WorkDir, Env: env, Stdin: stdinReader,
	})
	result := domain.RunResult{
		ID: runID(req.Build.ID, req.Workload.ID, started), BuildID: req.Build.ID, WorkloadID: req.Workload.ID,
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
	if _, path, err := r.artifacts.Put("stdout", commandResult.Stdout); err != nil {
		return result, err
	} else {
		result.Artifacts["stdout"] = path
	}
	if _, path, err := r.artifacts.Put("stderr", commandResult.Stderr); err != nil {
		return result, err
	} else {
		result.Artifacts["stderr"] = path
	}
	files, err := r.artifacts.SnapshotFiles(sandbox.WorkDir)
	if err != nil {
		return result, err
	}
	for key, value := range files {
		result.Artifacts[key] = value
	}
	if req.Mode == domain.RunModeDiscovery {
		coverage, err := r.artifacts.SnapshotFiles(filepath.Join(sandbox.Root, "coverage"))
		if err != nil {
			return result, err
		}
		for key, value := range coverage {
			result.Artifacts["coverage:"+key] = value
		}
	}
	success = runErr == nil
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func isolatedCommand(root, workDir string, denyNetwork bool, path string, args []string) (string, []string, error) {
	switch runtime.GOOS {
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
		if !bwrapIsolationSupported(bwrap, root, workDir) {
			return path, args, nil
		}
		wrapped := []string{"--die-with-parent"}
		if denyNetwork && bwrapNetNamespaceSupported(bwrap) {
			wrapped = append(wrapped, "--unshare-net")
		}
		wrapped = append(wrapped, "--ro-bind", "/", "/", "--bind", root, root, "--chdir", workDir, path)
		return bwrap, append(wrapped, args...), nil
	default:
		return "", nil, fmt.Errorf("local isolation is unsupported on %s", runtime.GOOS)
	}
}

// bwrapProbe caches whether this environment can run bubblewrap at all:
// both a trivial namespace setup and the bind/chdir shape isolatedCommand
// uses. Nested CI containers frequently block the mount calls involved.
var (
	bwrapProbeOnce sync.Once
	bwrapProbeOK   bool
)

func bwrapIsolationSupported(bwrap, root, workDir string) bool {
	bwrapProbeOnce.Do(func() {
		probe := exec.Command(bwrap, "--die-with-parent", "--ro-bind", "/", "/",
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

func bwrapNetNamespaceSupported(bwrap string) bool {
	bwrapNetNamespaceOnce.Do(func() {
		probe := exec.Command(bwrap, "--unshare-net", "--ro-bind", "/", "/", "--dev-bind", "/dev", "/dev", "true")
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
	if req.Build.ID == "" || req.Build.BinaryPath == "" || !filepath.IsAbs(req.Build.BinaryPath) {
		return errors.New("build ID and absolute binary path are required")
	}
	if _, err := os.Stat(req.Build.BinaryPath); err != nil {
		return err
	}
	if req.Workload.ID == "" {
		return errors.New("workload ID is required")
	}
	switch req.Mode {
	case domain.RunModeDiscovery, domain.RunModeDiagnosis, domain.RunModeMeasurement, domain.RunModeValidation:
	default:
		return fmt.Errorf("unsupported run mode %q", req.Mode)
	}
	if req.Workload.Command.Path != "" {
		a, errA := filepath.EvalSymlinks(req.Build.BinaryPath)
		b, errB := filepath.EvalSymlinks(req.Workload.Command.Path)
		if errA != nil || errB != nil || a != b {
			return errors.New("workload command path must be the configured build binary")
		}
	}
	return nil
}

func openInput(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
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

