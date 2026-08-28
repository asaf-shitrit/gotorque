package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// NetworkGuard is supplied by a container, VM, or CI integration. The local
// runner does not pretend it can disable networking by setting an
// environment variable.
type NetworkGuard interface {
	Prepare(root string) (cleanup func() error, err error)
}

// FilesystemGuard is supplied by a sandbox runtime (for example a container
// mount namespace). A process working directory alone cannot prevent writes to
// an absolute path, so restrictive workloads fail closed without this guard.
type FilesystemGuard interface {
	Prepare(root string) (cleanup func() error, err error)
}

type SandboxOptions struct {
	Root                 string
	NetworkDisabled      bool
	NetworkGuard         NetworkGuard
	FilesystemRestricted bool
	FilesystemGuard      FilesystemGuard
	KeepOnFailure        bool
}

type Sandbox struct {
	Root     string
	WorkDir  string
	TempDir  string
	HomeDir  string
	cleanups []func() error
	keep     bool
}

func NewSandbox(opts SandboxOptions) (*Sandbox, error) {
	root, err := createSandboxRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	s := &Sandbox{Root: root, WorkDir: filepath.Join(root, "work"), TempDir: filepath.Join(root, "tmp"), HomeDir: filepath.Join(root, "home"), keep: opts.KeepOnFailure}
	if err := mkdirSandboxDirs(s); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := attachSandboxGuards(s, opts); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return s, nil
}

func createSandboxRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("sandbox root is required")
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("sandbox root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, "run-")
}

func mkdirSandboxDirs(s *Sandbox) error {
	for _, dir := range []string{s.WorkDir, s.TempDir, s.HomeDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func attachSandboxGuards(s *Sandbox, opts SandboxOptions) error {
	if opts.NetworkDisabled {
		if err := attachGuard(s, opts.NetworkGuard, "network isolation requested but no network guard is configured"); err != nil {
			return err
		}
	}
	if opts.FilesystemRestricted {
		return attachGuard(s, opts.FilesystemGuard, "filesystem isolation requested but no filesystem guard is configured")
	}
	return nil
}

func attachGuard(s *Sandbox, guard interface {
	Prepare(root string) (cleanup func() error, err error)
}, missing string) error {
	if guard == nil {
		return fmt.Errorf("%s", missing)
	}
	cleanup, err := guard.Prepare(s.Root)
	if err != nil {
		return err
	}
	s.cleanups = append(s.cleanups, cleanup)
	return nil
}

func (s *Sandbox) Env() []string {
	return []string{
		"HOME=" + s.HomeDir,
		"TMPDIR=" + s.TempDir,
		"TMP=" + s.TempDir,
		"TEMP=" + s.TempDir,
	}
}

func (s *Sandbox) Close(success bool) error {
	if s == nil {
		return nil
	}
	var first error
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		if err := s.cleanups[i](); err != nil && first == nil {
			first = err
		}
	}
	if success || !s.keep {
		if err := os.RemoveAll(s.Root); err != nil && first == nil {
			first = err
		}
	}
	return first
}
