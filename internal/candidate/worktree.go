package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/toolchain"
)

type WorktreeManager struct {
	Toolchain  *toolchain.Toolchain
	Repository string
	Root       string
}

type Prepared struct {
	Candidate domain.Candidate
	Worktree  string
	manager   *WorktreeManager
}

func (m *WorktreeManager) Prepare(ctx context.Context, revision, patchPath, hypothesis string, policy Policy) (*Prepared, error) {
	if err := m.validatePrepare(patchPath); err != nil {
		return nil, err
	}
	data, err := m.loadPreparedPatch(patchPath)
	if err != nil {
		return nil, err
	}
	if _, err := ValidateUnifiedDiff(string(data), policy); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(append([]byte(revision+"\x00"), data...))
	id := hex.EncodeToString(digest[:])[:24]
	path := filepath.Join(m.Root, "candidate-"+id)
	if _, err := m.Toolchain.CreateWorktree(ctx, m.Repository, path, revision); err != nil {
		return nil, fmt.Errorf("create candidate worktree: %w", err)
	}
	prepared := &Prepared{Candidate: domain.Candidate{ID: id, BaseRevision: revision, Hypothesis: hypothesis, PatchPath: patchPath, CreatedAt: time.Now().UTC()}, Worktree: path, manager: m}
	if err := m.applyPreparedPatch(ctx, prepared, path, patchPath); err != nil {
		return nil, err
	}
	if err := rejectProtectedChanges(ctx, m.Toolchain, path); err != nil {
		_ = prepared.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return prepared, nil
}

// rejectProtectedChanges judges the applied worktree, not the diff that
// produced it. Header validation reads the patch text, and GNU patch, the
// fuzzy fallback, picks its target by its own rules: handed `--- a/go.mod`
// and `+++ b/other.go` it rewrote go.mod. Git's list of what actually changed
// is the only record that does not depend on how a tool read the diff, so a
// candidate that reached a protected path by any route stops here, before a
// build or a test run could be judged against a gate it weakened.
func rejectProtectedChanges(ctx context.Context, chain *toolchain.Toolchain, worktree string) error {
	changed, err := chain.ChangedFiles(ctx, worktree)
	if err != nil {
		return fmt.Errorf("list files the patch changed: %w", err)
	}
	for _, name := range changed {
		if err := rejectPatchPath(name); err != nil {
			return fmt.Errorf("applied patch changed a protected path: %w", err)
		}
		if err := rejectSymlink(worktree, name); err != nil {
			return err
		}
	}
	return nil
}

// rejectSymlink refuses a changed path that is now a symbolic link. Header
// validation refuses symlink modes, but only in the headers it can read; a
// link left in the tree would let the build or the test suite read a
// protected file, or a file outside the worktree, under a harmless name.
func rejectSymlink(worktree, name string) error {
	info, err := os.Lstat(filepath.Join(worktree, filepath.FromSlash(name)))
	if errors.Is(err, fs.ErrNotExist) {
		// A deleted path has nothing left to follow.
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect changed path %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("applied patch left %q as a symbolic link; only regular files may be patched", name)
	}
	return nil
}

func (m *WorktreeManager) validatePrepare(patchPath string) error {
	if m.Toolchain == nil {
		return errors.New("toolchain is required")
	}
	if !filepath.IsAbs(m.Repository) || !filepath.IsAbs(m.Root) || !filepath.IsAbs(patchPath) {
		return errors.New("repository, worktree root, and patch path must be absolute")
	}
	return nil
}

func (m *WorktreeManager) loadPreparedPatch(patchPath string) ([]byte, error) {
	data, err := os.ReadFile(patchPath)
	if err != nil {
		return nil, err
	}
	if normalized, changed := NormalizeUnifiedDiff(string(data)); changed {
		data = []byte(normalized)
		if err := os.WriteFile(patchPath, data, 0o600); err != nil {
			return nil, fmt.Errorf("write normalized patch: %w", err)
		}
	}
	// Models drop leading directories from diff paths (dedupe/main.go for
	// cmd/dedupe/main.go); when the suffix identifies exactly one file the
	// headers are rewritten so the patch can apply.
	if remapped, changed := RemapDiffPaths(m.Repository, data); changed {
		data = remapped
		_ = os.WriteFile(patchPath, data, 0o600)
	}
	return data, nil
}

func (m *WorktreeManager) applyPreparedPatch(ctx context.Context, prepared *Prepared, path, patchPath string) error {
	checkResult, checkErr := m.Toolchain.ApplyPatchCheck(ctx, path, patchPath)
	if checkErr != nil {
		// Model-generated diffs often carry approximate context. Fall back
		// to GNU patch fuzz matching; Prepare then checks which files the
		// fallback actually changed, since it may not be the ones validated.
		if _, fuzzyErr := m.Toolchain.ApplyPatchFuzzy(ctx, path, patchPath); fuzzyErr != nil {
			_ = prepared.Close(context.WithoutCancel(ctx))
			return fmt.Errorf("git apply check: %w%s", checkErr, stderrSuffix(checkResult.Stderr))
		}
		return nil
	}
	if applyResult, applyErr := m.Toolchain.ApplyPatch(ctx, path, patchPath); applyErr != nil {
		_ = prepared.Close(context.WithoutCancel(ctx))
		return fmt.Errorf("apply candidate: %w%s", applyErr, stderrSuffix(applyResult.Stderr))
	}
	return nil
}

// stderrSuffix renders captured command stderr as an error suffix so the
// model that produced a rejected diff can see why git apply or patch failed.
func stderrSuffix(stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return ""
	}
	if len(text) > 400 {
		text = text[len(text)-400:]
	}
	return ": " + text
}

func (p *Prepared) Close(ctx context.Context) error {
	if p == nil || p.manager == nil || p.Worktree == "" {
		return nil
	}
	if rel, err := filepath.Rel(p.manager.Root, p.Worktree); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("candidate worktree is outside configured root")
	}
	_, err := p.manager.Toolchain.RemoveWorktree(ctx, p.manager.Repository, p.Worktree)
	if err == nil {
		p.Worktree = ""
	}
	return err
}
