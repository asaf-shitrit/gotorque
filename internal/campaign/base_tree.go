package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// baseTree is a pristine checkout of the campaign's base revision, owned by
// candidate evaluation, that source is read from when a diff is built from
// function_source, function_sources or a null patch (ADR 0022, ADR 0036).
//
// Those transports used to read the target's file from the campaign's
// canonical checkout, which is clean at the base revision only for as long as
// nothing touches it: miller's tests rewrote tracked fixtures under test/input
// and left seven of them truncated in the very checkout the next diff was
// built against. A diff built from a dirty tree applies to nothing but that
// tree. The base tree is created in the campaign directory, nothing runs in
// it, and it is checked on use and created again when it is not exactly the
// base revision with no change, so what a diff is built against does not
// depend on what else touched the canonical checkout.
type baseTree struct {
	toolchain  *toolchain.Toolchain
	repository string
	revision   string
	dir        string
	// verified remembers that this evaluation already checked the tree, so a
	// multi-file diff does not ask Git about it once per file.
	verified bool
}

func newBaseTree(tc *toolchain.Toolchain, campaignDir, repository, revision string) *baseTree {
	return &baseTree{toolchain: tc, repository: repository, revision: revision, dir: filepath.Join(campaignDir, "worktrees", "base-tree")}
}

// root returns the base tree's path, creating it first if it is missing, is
// not at the base revision, or has any change, tracked or not.
func (b *baseTree) root(ctx context.Context) (string, error) {
	if b.verified {
		return b.dir, nil
	}
	if !b.pristine(ctx) {
		if err := b.recreate(ctx); err != nil {
			return "", err
		}
	}
	b.verified = true
	return b.dir, nil
}

func (b *baseTree) pristine(ctx context.Context) bool {
	if info, err := os.Stat(filepath.Join(b.dir, ".git")); err != nil || info.IsDir() {
		return false
	}
	status, err := b.toolchain.GitStatus(ctx, b.dir)
	if err != nil || len(strings.TrimSpace(string(status.Stdout))) != 0 {
		return false
	}
	head, err := b.toolchain.GitRevision(ctx, b.dir)
	return err == nil && strings.TrimSpace(string(head.Stdout)) == b.revision
}

func (b *baseTree) recreate(ctx context.Context) error {
	_, _ = b.toolchain.RemoveWorktree(ctx, b.repository, b.dir)
	if err := os.RemoveAll(b.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.dir), 0o700); err != nil {
		return err
	}
	if _, err := b.toolchain.CreateWorktree(ctx, b.repository, b.dir, b.revision); err != nil {
		return fmt.Errorf("check out the base revision for source transports: %w", err)
	}
	return nil
}

// remove deletes the base tree and its registration in the repository. A tree
// that was never created costs no Git call.
func (b *baseTree) remove(ctx context.Context) {
	if _, err := os.Stat(b.dir); err != nil {
		return
	}
	_, _ = b.toolchain.RemoveWorktree(ctx, b.repository, b.dir)
	_ = os.RemoveAll(b.dir)
}

// releaseBase removes the base tree, if this evaluation created one, even when
// the caller's context is already canceled (duration budget or Ctrl-C).
func (ev *evaluator) releaseBase(ctx context.Context) {
	ev.base.remove(context.WithoutCancel(ctx))
}

// baseRoot is the path of the pristine base tree, for reading the source a
// diff is built against.
func (ev *evaluator) baseRoot(ctx context.Context) (string, error) {
	return ev.base.root(ctx)
}
