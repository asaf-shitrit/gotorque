package toolchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Toolchain exposes only the Go, Git, benchstat, pprof, and trace commands the
// harness needs. It exposes no Run(string) or shell equivalent.
type Toolchain struct {
	executor      Executor
	goPath        string
	gitPath       string
	benchstatPath string
	benchstatOnce sync.Once
	benchstatErr  error
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
	// JSON asks for `go test -json`, whose per-test outcomes the behavior
	// gate compares against the unpatched revision's.
	JSON       bool
	Bench      string
	Count      int
	CoverDir   string
	TraceFile  string
	Cpuprofile string
	Env        []string
}

func (t *Toolchain) Test(ctx context.Context, req TestRequest) (Result, error) {
	if err := requireDirectory(req.Repository); err != nil {
		return Result{}, err
	}
	if req.Cpuprofile != "" && !filepath.IsAbs(req.Cpuprofile) {
		return Result{}, errors.New("cpuprofile path must be absolute")
	}
	env := append([]string(nil), req.Env...)
	if req.CoverDir != "" {
		env = append(env, "GOCOVERDIR="+req.CoverDir)
	}
	return t.run(ctx, t.goPath, testArgs(req), req.Repository, env, nil)
}

// testArgs renders the `go test` argument list for one request.
func testArgs(req TestRequest) []string {
	args := []string{"test", "-mod=readonly"}
	if req.Race {
		args = append(args, "-race")
	}
	if req.JSON {
		args = append(args, "-json")
	}
	if req.Bench != "" {
		args = append(args, "-run=^$", "-bench", req.Bench, "-benchmem")
	}
	if req.Count > 0 {
		args = append(args, "-count", strconv.Itoa(req.Count))
	}
	if req.TraceFile != "" {
		args = append(args, "-trace", req.TraceFile)
	}
	if req.Cpuprofile != "" {
		args = append(args, "-cpuprofile", req.Cpuprofile)
	}
	if len(req.Packages) == 0 {
		return append(args, "./...")
	}
	return append(args, req.Packages...)
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

// ChangedFiles lists every path in repository whose content differs from its
// checked-out revision: modified, deleted, and new files, untracked and
// ignored ones included, each named repository-relative.
//
// It exists because the patch a candidate proposes is not the change that
// lands. GNU patch, the fuzzy fallback, chooses the file it edits by its own
// rules: given `--- a/go.mod` and `+++ b/other.go` it rewrote go.mod, a path
// that header validation never saw as a target. Asking Git what actually
// changed is the only view that does not depend on how a tool read the diff.
// Ignored files are listed because a new file under a .gitignore pattern is
// otherwise invisible, and renames are split into their two paths so a moved
// file is judged at both ends.
func (t *Toolchain) ChangedFiles(ctx context.Context, repository string) ([]string, error) {
	if err := requireDirectory(repository); err != nil {
		return nil, err
	}
	result, err := t.run(ctx, t.gitPath, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored", "--no-renames"}, repository, nil, nil)
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(result.Stdout)
}

// ChangedLines returns the applied change as a zero-context diff against the
// checked-out revision, so the lines a patch touched can be read from what
// actually landed rather than from the patch text a fuzzy apply may have
// placed elsewhere. External diff drivers and color are disabled so the
// output is always plain unified diff.
func (t *Toolchain) ChangedLines(ctx context.Context, repository string) ([]byte, error) {
	if err := requireDirectory(repository); err != nil {
		return nil, err
	}
	result, err := t.run(ctx, t.gitPath, []string{"diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "HEAD"}, repository, nil, nil)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

// parsePorcelainZ reads `git status --porcelain=v1 -z` output. Each entry is a
// two-letter status, a space, and a path, NUL-terminated and never quoted; a
// rename or copy entry is followed by a second NUL-terminated field holding
// its source path, which is returned as a changed path too.
func parsePorcelainZ(output []byte) ([]string, error) {
	fields := strings.Split(string(output), "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("malformed git status entry %q", entry)
		}
		paths = append(paths, entry[3:])
		if !isRenameOrCopy(entry[:2]) {
			continue
		}
		i++
		if i >= len(fields) || fields[i] == "" {
			return nil, fmt.Errorf("git status entry %q is missing its source path", entry)
		}
		paths = append(paths, fields[i])
	}
	return paths, nil
}

func isRenameOrCopy(status string) bool {
	return strings.ContainsAny(status, "RC")
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
	return t.run(ctx, t.gitPath, []string{"apply", "--check", "--unidiff-zero", patchPath}, repository, nil, nil)
}

// ApplyPatch applies a patch only after the caller has passed candidate policy
// checks. It accepts an absolute artifact path, never patch text or a shell.
//
// Both it and ApplyPatchCheck pass --unidiff-zero. Without it git anchors a
// hunk that has no trailing context to the end of the file, and one with no
// leading context to the start, and models end hunks on their last changed
// line: on a live gojq campaign two correct bufio patches of printValues, the
// middle of a 400-line file, failed with "patch does not apply". The flag
// drops only that anchoring; every context line still has to match.
func (t *Toolchain) ApplyPatch(ctx context.Context, repository, patchPath string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(patchPath) {
		return Result{}, errors.New("patch path must be absolute")
	}
	return t.run(ctx, t.gitPath, []string{"apply", "--unidiff-zero", "--whitespace=error-all", patchPath}, repository, nil, nil)
}

// ApplyPatchFuzzy applies a patch with GNU patch's fuzz matching for models
// that cannot reproduce exact context lines from memory. It is only used as
// a fallback after strict git apply fails. patch picks the file it edits by
// its own rules, which need not be the path a header validator checked, so
// the caller must judge the result with ChangedFiles, not the diff text.
func (t *Toolchain) ApplyPatchFuzzy(ctx context.Context, repository, patchPath string) (Result, error) {
	if err := requireDirectory(repository); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(patchPath) {
		return Result{}, errors.New("patch path must be absolute")
	}
	data, err := os.ReadFile(patchPath)
	if err != nil {
		return Result{}, err
	}
	return t.run(ctx, patchBinary(), []string{"-p1", "--fuzz=5", "--no-backup-if-mismatch", "--silent"}, repository, nil, bytes.NewReader(data))
}

func patchBinary() string {
	if resolved, err := exec.LookPath("patch"); err == nil {
		return resolved
	}
	return "/usr/bin/patch"
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

// HasBenchstat reports whether the configured benchstat binary is resolvable.
// The LookPath result is cached after the first probe so a missing binary does
// not get re-looked-up on every workload; callers must still treat every
// Benchstat invocation as fallible.
func (t *Toolchain) HasBenchstat() bool {
	t.benchstatOnce.Do(func() {
		_, t.benchstatErr = exec.LookPath(t.benchstatPath)
	})
	return t.benchstatErr == nil
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

// PprofList returns the source-annotated listing for one function from a
// pprof profile: go tool pprof -list prints the routine's file, start line,
// and per-line samples when the profile carries symbolization.
func (t *Toolchain) PprofList(ctx context.Context, functionName, profilePath string) (Result, error) {
	if !filepath.IsAbs(profilePath) {
		return Result{}, errors.New("profile path must be absolute")
	}
	return t.run(ctx, t.goPath, []string{"tool", "pprof", "-list", functionName, profilePath}, "", nil, nil)
}

// TracePprof asks the authoritative Go trace tool to convert a trace into a
// pprof-compatible aggregate. Valid kinds are enumerated.
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
	base := os.Environ()
	// git resolves a repository, index, and worktree from these variables ahead
	// of the working directory, so an inherited value would silently redirect a
	// command at a different checkout than the one Dir names.
	if path == t.gitPath {
		base = withoutGitScoping(base)
	}
	result, err := t.executor.Run(ctx, Invocation{Path: path, Args: args, Dir: dir, Env: mergeEnvironment(base, env), Stdin: stdin})
	if err != nil {
		return result, fmt.Errorf("%s %s: %w", path, strings.Join(args, " "), err)
	}
	return result, nil
}

// gitScopingEnv lists the variables git uses to select a repository, index, or
// worktree independently of the working directory. Every git call in this
// package is pointed at a target checkout with Dir, so a value inherited from
// the caller must not override that: inside a git hook git exports
// GIT_INDEX_FILE and GIT_PREFIX, and a caller working in another repository may
// export GIT_DIR or GIT_WORK_TREE. Dropping them keeps the target checkout the
// only repository in play, which is what makes these wrappers safe to call from
// a commit hook or from within another checkout.
var gitScopingEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_PREFIX",
	"GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
}

// GitScopingEnv returns the variables that redirect git away from the
// directory it runs in. Test binaries that run git directly to build fixtures
// clear them in TestMain, for the reason the wrappers here drop them: inside a
// commit hook they name the repository being committed.
func GitScopingEnv() []string { return slices.Clone(gitScopingEnv) }

func withoutGitScoping(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, pair := range env {
		key, _, _ := strings.Cut(pair, "=")
		if slices.Contains(gitScopingEnv, key) {
			continue
		}
		kept = append(kept, pair)
	}
	return kept
}

func mergeEnvironment(base, overrides []string) []string {
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
