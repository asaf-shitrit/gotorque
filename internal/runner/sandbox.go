package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// NetworkGuard is supplied by a container, VM, or CI integration. The local
// runner deliberately does not pretend it can disable networking by setting an
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
	if opts.Root == "" {
		return nil, errors.New("sandbox root is required")
	}
	if !filepath.IsAbs(opts.Root) {
		return nil, errors.New("sandbox root must be absolute")
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(opts.Root, "run-")
	if err != nil {
		return nil, err
	}
	s := &Sandbox{Root: root, WorkDir: filepath.Join(root, "work"), TempDir: filepath.Join(root, "tmp"), HomeDir: filepath.Join(root, "home"), keep: opts.KeepOnFailure}
	for _, dir := range []string{s.WorkDir, s.TempDir, s.HomeDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	if opts.NetworkDisabled {
		if opts.NetworkGuard == nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("network isolation requested but no network guard is configured")
		}
		cleanup, err := opts.NetworkGuard.Prepare(root)
		if err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
		s.cleanups = append(s.cleanups, cleanup)
	}
	if opts.FilesystemRestricted {
		if opts.FilesystemGuard == nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("filesystem isolation requested but no filesystem guard is configured")
		}
		cleanup, err := opts.FilesystemGuard.Prepare(root)
		if err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
		s.cleanups = append(s.cleanups, cleanup)
	}
	return s, nil
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
