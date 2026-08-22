package toolchain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Toolchain exposes only the Go, Git, benchstat, pprof, and trace commands the
// harness needs. It deliberately has no Run(string) or shell equivalent.
type Toolchain struct {
	executor      Executor
	goPath        string
	gitPath       string
	benchstatPath string
}

type Options struct {
	Executor      Executor
	GoPath        string
	GitPath       string
	BenchstatPath string
}

func New(opts Options) *Toolchain {
	if opts.Executor == nil {
		opts.Executor = OSExecutor{}
	}
	if opts.GoPath == "" {
		opts.GoPath = "go"
	}
	if opts.GitPath == "" {
		opts.GitPath = "git"
	}
	if opts.BenchstatPath == "" {
		opts.BenchstatPath = "benchstat"
	}
	return &Toolchain{executor: opts.Executor, goPath: opts.GoPath, gitPath: opts.GitPath, benchstatPath: opts.BenchstatPath}
}

type BuildRequest struct {
	Repository string
	Target     string
	Output     string
	Tags       []string
	LDFlags    []string
	PGOProfile string
	Cover      bool
	Env        []string
}

func (t *Toolchain) Build(ctx context.Context, req BuildRequest) (Result, error) {
	if err := requireDirectory(req.Repository); err != nil {
		return Result{}, err
	}
	if req.Target == "" || req.Output == "" {
		return Result{}, errors.New("build target and output are required")
	}
	if !filepath.IsAbs(req.Output) {
		return Result{}, errors.New("build output must be an absolute path")
	}
	args := []string{"build", "-mod=readonly", "-trimpath", "-o", req.Output}
	if req.Cover {
		args = append(args, "-cover")
	}
	if len(req.Tags) > 0 {
		args = append(args, "-tags", strings.Join(req.Tags, ","))
	}
	if len(req.LDFlags) > 0 {
		args = append(args, "-ldflags", strings.Join(req.LDFlags, " "))
	}
	if req.PGOProfile != "" {
		args = append(args, "-pgo", req.PGOProfile)
	}
	args = append(args, req.Target)
	return t.run(ctx, t.goPath, args, req.Repository, req.Env, nil)
}

type TestRequest struct {
	Repository string
	Packages   []string
	Race       bool
	Bench      string
	Count      int
	CoverDir   string
	TraceFile  string
	Env        []string
}

func (t *Toolchain) Test(ctx context.Context, req TestRequest) (Result, error) {
	if err := requireDirectory(req.Repository); err != nil {
		return Result{}, err
	}
	args := []string{"test", "-mod=readonly"}
	if req.Race {
		args = append(args, "-race")
	}
	if req.Bench != "" {
		args = append(args, "-run=^$", "-bench", req.Bench, "-benchmem")
	}
	if req.Count > 0 {
		args = append(args, "-count", fmt.Sprint(req.Count))
	}
	if req.TraceFile != "" {
		args = append(args, "-trace", req.TraceFile)
	}
	env := append([]string(nil), req.Env...)
	if req.CoverDir != "" {
		env = append(env, "GOCOVERDIR="+req.CoverDir)
	}
	if len(req.Packages) == 0 {
		args = append(args, "./...")
	} else {
		args = append(args, req.Packages...)
	}
	return t.run(ctx, t.goPath, args, req.Repository, env, nil)
}

func (t *Toolchain) GoEnv(ctx context.Context, repository string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	return t.run(ctx, t.goPath, []string{"env", "-json"}, repository, nil, nil)
}

func (t *Toolchain) GitRevision(ctx context.Context, repository string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	return t.run(ctx, t.gitPath, []string{"rev-parse", "HEAD"}, repository, nil, nil)
}

// GitStatus returns porcelain-v1 output including untracked files. An empty
// stdout is the only state safe for a campaign against a canonical checkout.
func (t *Toolchain) GitStatus(ctx context.Context, repository string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	return t.run(ctx, t.gitPath, []string{"status", "--porcelain=v1", "--untracked-files=all"}, repository, nil, nil)
}

// GoVersion resolves the locally installed toolchain. GOTOOLCHAIN=local
// prevents Go from downloading or selecting a different toolchain.
func (t *Toolchain) GoVersion(ctx context.Context, repository string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	return t.run(ctx, t.goPath, []string{"version"}, repository, []string{"GOTOOLCHAIN=local"}, nil)
}

// GoList inventories packages without permitting module graph changes.
func (t *Toolchain) GoList(ctx context.Context, repository string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	return t.run(ctx, t.goPath, []string{"list", "-mod=readonly", "-json", "./..."}, repository, []string{"GOTOOLCHAIN=local"}, nil)
}

func (t *Toolchain) CreateWorktree(ctx context.Context, repository, path, revision string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(path) || revision == "" {
		return Result{}, errors.New("absolute worktree path and revision are required")
	}
	return t.run(ctx, t.gitPath, []string{"worktree", "add", "--detach", path, revision}, repository, nil, nil)
}

func (t *Toolchain) ApplyPatchCheck(ctx context.Context, repository, patchPath string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(patchPath) {
		return Result{}, errors.New("patch path must be absolute")
	}
	return t.run(ctx, t.gitPath, []string{"apply", "--check", patchPath}, repository, nil, nil)
}

// ApplyPatch applies a patch only after the caller has passed candidate policy
// checks. It accepts an absolute artifact path, never patch text or a shell.
func (t *Toolchain) ApplyPatch(ctx context.Context, repository, patchPath string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(patchPath) {
		return Result{}, errors.New("patch path must be absolute")
	}
	return t.run(ctx, t.gitPath, []string{"apply", "--whitespace=error-all", patchPath}, repository, nil, nil)
}

// RemoveWorktree removes one explicitly identified disposable worktree and
// prunes its administrative record from the canonical repository.
func (t *Toolchain) RemoveWorktree(ctx context.Context, repository, path string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(path) {
		return Result{}, errors.New("worktree path must be absolute")
	}
	return t.run(ctx, t.gitPath, []string{"worktree", "remove", "--force", path}, repository, nil, nil)
}

func (t *Toolchain) Benchstat(ctx context.Context, baseline, candidate string) (Result, error) {
	if !filepath.IsAbs(baseline) || !filepath.IsAbs(candidate) {
		return Result{}, errors.New("benchstat inputs must be absolute paths")
	}
	return t.run(ctx, t.benchstatPath, []string{baseline, candidate}, "", nil, nil)
}

func (t *Toolchain) PprofTop(ctx context.Context, profilePath string, nodeCount int) (Result, error) {
	if !filepath.IsAbs(profilePath) {
		return Result{}, errors.New("profile path must be absolute")
	}
	if nodeCount <= 0 {
		nodeCount = 50
	}
	return t.run(ctx, t.goPath, []string{"tool", "pprof", "-top", "-cum", fmt.Sprintf("-nodecount=%d", nodeCount), profilePath}, "", nil, nil)
}

// TracePprof asks the authoritative Go trace tool to convert a trace into a
// pprof-compatible aggregate. Valid kinds are deliberately enumerated.
func (t *Toolchain) TracePprof(ctx context.Context, tracePath, kind string) (Result, error) {
	if !filepath.IsAbs(tracePath) {
		return Result{}, errors.New("trace path must be absolute")
	}
	switch kind {
	case "net", "sync", "syscall", "sched":
	default:
		return Result{}, fmt.Errorf("unsupported trace profile kind %q", kind)
	}
	return t.run(ctx, t.goPath, []string{"tool", "trace", "-pprof=" + kind, tracePath}, "", nil, nil)
}

func (t *Toolchain) run(ctx context.Context, path string, args []string, dir string, env []string, stdin io.Reader) (Result, error) {
	result, err := t.executor.Run(ctx, Invocation{Path: path, Args: args, Dir: dir, Env: mergeEnvironment(env), Stdin: stdin})
	if err != nil {
		return result, fmt.Errorf("%s %s: %w", path, strings.Join(args, " "), err)
	}
	return result, nil
}

func mergeEnvironment(overrides []string) []string {
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, pair := range base {
		key, _, _ := strings.Cut(pair, "=")
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		_, value, _ := strings.Cut(pair, "=")
		values[key] = value
	}
	for _, pair := range overrides {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			continue
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = value
	}
	merged := make([]string, 0, len(order))
	for _, key := range order {
		merged = append(merged, key+"="+values[key])
	}
	return merged
}

func requireDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("repository path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}
