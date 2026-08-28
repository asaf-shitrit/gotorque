// Package toolchain provides small, allowlisted wrappers around the Go
// toolchain and Git. It intentionally does not expose a general shell API.
package toolchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// Invocation is internal plumbing for the fixed commands in this package.
// Callers outside toolchain should use Go, Git, or Benchstat methods instead.
type Invocation struct {
	Path      string
	Args      []string
	Dir       string
	Env       []string
	Stdin     io.Reader
	MaxOutput int64
}

type Result struct {
	Stdout      []byte
	Stderr      []byte
	ExitCode    int
	Started     time.Time
	Duration    time.Duration
	UserCPU     time.Duration
	SystemCPU   time.Duration
	MaxRSSBytes int64
}

// Executor makes command wrappers testable without executing processes.
type Executor interface {
	Run(context.Context, Invocation) (Result, error)
}

// OSExecutor kills the entire process group on cancellation. This matters for
// Go commands that start compiler, test, or helper subprocesses.
type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, in Invocation) (Result, error) {
	if in.Path == "" {
		return Result{}, errors.New("command path is required")
	}
	cmd := exec.Command(in.Path, in.Args...)
	cmd.Dir = in.Dir
	cmd.Env = in.Env
	cmd.Stdin = normalizeStdin(in.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	limit := in.MaxOutput
	if limit <= 0 {
		limit = 16 << 20 // Enough for diagnostics while bounding an errant tool.
	}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return Result{Started: started}, err
	}
	err := waitOrCancel(ctx, cmd)
	result := Result{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode(err),
		Started: started, Duration: time.Since(started),
	}
	fillResourceUsage(&result, cmd.ProcessState)
	return result, commandError(ctx, err, stdout, stderr, limit)
}

func normalizeStdin(stdin io.Reader) io.Reader {
	if file, ok := stdin.(*os.File); ok && file == nil {
		// Treat a typed-nil file as no input so the child keeps a valid
		// descriptor 0 instead of an invalid one.
		return nil
	}
	return stdin
}

func waitOrCancel(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return finishCanceled(ctx, cmd, done)
	}
}

func finishCanceled(ctx context.Context, cmd *exec.Cmd, done <-chan error) error {
	terminateProcessGroup(cmd.Process)
	var err error
	select {
	case err = <-done:
	case <-time.After(250 * time.Millisecond):
		killProcessGroup(cmd.Process)
		err = <-done
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func fillResourceUsage(result *Result, state *os.ProcessState) {
	if state == nil {
		return
	}
	result.UserCPU = state.UserTime()
	result.SystemCPU = state.SystemTime()
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return
	}
	// Darwin reports bytes while Linux reports KiB. A CI adapter can
	// normalize platform metadata; preserve the operating-system value
	// conservatively here rather than claim a false cross-platform unit.
	result.MaxRSSBytes = usage.Maxrss
	if runtime.GOOS == "linux" {
		result.MaxRSSBytes *= 1024
	}
}

func commandError(ctx context.Context, err error, stdout, stderr *limitedBuffer, limit int64) error {
	if stdout.exceeded || stderr.exceeded {
		return fmt.Errorf("command output exceeded %d byte limit", limit)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func terminateProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	if runningOnUnix() {
		// Negative pid addresses the process group created with Setpgid. The
		// initial interrupt gives Go test binaries a chance to flush profiles.
		_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
		return
	}
	_ = p.Kill()
}

func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	if runningOnUnix() {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		return
	}
	_ = p.Kill()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int64
	written  int64
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.written += int64(len(p))
	if b.written > b.limit {
		remaining := b.limit - (b.written - int64(len(p)))
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		b.exceeded = true
		return len(p), nil // let the child finish so it can be cleaned up normally
	}
	return b.Buffer.Write(p)
}

func runningOnUnix() bool { return runtime.GOOS != "windows" }
