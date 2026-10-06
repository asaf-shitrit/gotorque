package campaign

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// A null candidate is a patch that changes nothing a workload can observe: it
// adds one comment line after the package clause of a file in the target's
// build package. The compiled code is identical; only line numbers after the
// comment move, the smallest real change a binary can carry. A sound harness
// accepts none of them and rejects almost none, so a run of them measures the
// false-acceptance and false-rejection rates directly (stability criterion
// 2). Overnight on 2026-09-28 the order bias fixed by ABBA (#31) would have
// shown up here as guardrail rejections on miller.

// runNullCandidates evaluates null candidates until the campaign has
// NullCandidates records, through the same evaluation and policy as any
// candidate, and returns the stop reason.
func (e *Engine) runNullCandidates(ctx context.Context) (string, error) {
	files, err := nullTargetFiles(e.state.Repository, e.state.Manifest.Target.Build.Directory, e.state.Manifest.Target.Build.Package)
	if err != nil {
		return "", err
	}
	for len(e.state.CandidateRecords) < e.state.NullCandidates {
		// A spent max_duration cancels ctx. Every attempt after that failed
		// on the dead context (a canceled measurement, then worktrees git
		// could not create) and was recorded as a rejection: null-gron
		// reported 4 false rejections that were the bound. Returning the
		// context's error stops the campaign as interrupted and resumable,
		// the way the agent path does.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		attempt := len(e.state.CandidateRecords) + 1
		file := files[(attempt-1)%len(files)]
		patch, err := nullPatch(e.state.Repository, file, attempt)
		if err != nil {
			return "", err
		}
		evidence, err := e.evaluateCandidate(ctx, orchestrator.CandidateRequest{
			Campaign: e.campaignRequest(), Attempt: attempt,
			Proposal: agents.OptimizerResult{Patch: patch, Hypothesis: fmt.Sprintf("null candidate %d: a comment line in %s, no code change", attempt, file)},
		})
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, err := e.recordVerdict(attempt, evidence, nil, agents.ReviewerResult{}); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%d null candidates evaluated", e.state.NullCandidates), nil
}

// nullTargetFiles lists the non-test Go files of the build package,
// repository-relative and sorted.
func nullTargetFiles(repo, buildDir, pkg string) ([]string, error) {
	dir := filepath.Join(repo, filepath.FromSlash(buildDir), filepath.FromSlash(pkg))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list the build package for null candidates: %w", err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, err := filepath.Rel(repo, filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		files = append(files, filepath.ToSlash(rel))
	}
	if len(files) == 0 {
		return nil, errors.New("the build package has no Go file to place a null candidate in")
	}
	sort.Strings(files)
	return files, nil
}

// nullPatch is a unified diff adding "// gotorque null candidate N" after
// file's package clause.
func nullPatch(repo, file string, attempt int) (string, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(file)))
	if err != nil {
		return "", err
	}
	line, text, ok := packageClause(data)
	if !ok {
		return "", fmt.Errorf("%s has no package clause", file)
	}
	return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ -%d,1 +%d,2 @@\n %s\n+// gotorque null candidate %d\n", file, file, line, line, text, attempt), nil
}

// packageClause finds the first line that starts with "package ".
func packageClause(data []byte) (int, string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; scanner.Scan(); n++ {
		if text := scanner.Text(); strings.HasPrefix(text, "package ") {
			return n, text, true
		}
	}
	return 0, "", false
}
