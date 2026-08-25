package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	if m.Toolchain == nil {
		return nil, errors.New("toolchain is required")
	}
	if !filepath.IsAbs(m.Repository) || !filepath.IsAbs(m.Root) || !filepath.IsAbs(patchPath) {
		return nil, errors.New("repository, worktree root, and patch path must be absolute")
	}
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
	checkResult, checkErr := m.Toolchain.ApplyPatchCheck(ctx, path, patchPath)
	if checkErr != nil {
		// Model-generated diffs often carry approximate context. Fall back
		// to GNU patch fuzz matching; the applied tree still faces the full
		// test-suite behavior gate before any measurement.
		if _, fuzzyErr := m.Toolchain.ApplyPatchFuzzy(ctx, path, patchPath); fuzzyErr != nil {
			_ = prepared.Close(context.Background())
			return nil, fmt.Errorf("git apply check: %w%s", checkErr, stderrSuffix(checkResult.Stderr))
		}
	} else if applyResult, applyErr := m.Toolchain.ApplyPatch(ctx, path, patchPath); applyErr != nil {
		_ = prepared.Close(context.Background())
		return nil, fmt.Errorf("apply candidate: %w%s", applyErr, stderrSuffix(applyResult.Stderr))
	}
	return prepared, nil
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
