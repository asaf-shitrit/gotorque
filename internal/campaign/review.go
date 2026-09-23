package campaign

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/orchestrator"
	"google.golang.org/genai"
)

// ReviewerJev marks a campaign whose reviewer was Jev behaviour-hazard checks
// rather than a model role.
const ReviewerJev = "jev"

// reviewAnalyst is the deterministic reviewer selected by --reviewer jev. It
// asks Jev one yes/no question per behaviour hazard about the patch and raises
// the hazards whose answers stand well above Jev's usual answers on real,
// merged performance patches. Its result is advice: it is recorded with the
// verdict and shown to the next cycle, and the policy never reads it.
type reviewAnalyst struct {
	engine    *Engine
	evaluator jev.Evaluator
	usage     *agents.UsageCollector
}

func (a reviewAnalyst) ReviewPatch(ctx context.Context, req orchestrator.ReviewRequest) (agents.ReviewerResult, error) {
	patch := req.Proposal.Patch
	if strings.TrimSpace(patch) == "" {
		return agents.ReviewerResult{BehaviorArgument: "no patch to review"}, nil
	}
	fn := patchedFunction(req.Campaign.Repository, patch)
	resp, err := a.evaluator.Evaluate(ctx, jev.Request{State: jev.ReviewState(req.Proposal.Hypothesis, fn.Name, fn.Source, patch), Questions: jev.ReviewQuestions()})
	if err != nil {
		return agents.ReviewerResult{}, err
	}
	a.usage.Record(string(agents.RoleReviewer), &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     tokens32(resp.Usage.InputTokens),
		CandidatesTokenCount: tokens32(resp.Usage.OutputTokens),
		TotalTokenCount:      tokens32(resp.Usage.InputTokens + resp.Usage.OutputTokens),
	})
	scores, err := jev.RankHazards(resp.Answers)
	if err != nil {
		return agents.ReviewerResult{}, err
	}
	flagged := jev.FlaggedHazards(scores)
	if a.engine != nil {
		_ = a.engine.saveEvent("patch_review", fmt.Sprintf("Jev raised %d behaviour hazard(s) on %s", len(flagged), fn.Name), scores)
	}
	return reviewerResult(flagged), nil
}

func reviewerResult(flagged []jev.HazardScore) agents.ReviewerResult {
	if len(flagged) == 0 {
		return agents.ReviewerResult{Proceed: true, BehaviorArgument: "Jev raised no behaviour hazard: every answer was within its usual range for real, merged performance patches"}
	}
	result := agents.ReviewerResult{BehaviorArgument: fmt.Sprintf("Jev raised %d behaviour hazard(s); the verdict still rests on measurement and the behaviour gate", len(flagged))}
	for _, s := range flagged {
		result.Concerns = append(result.Concerns, fmt.Sprintf("%s (Jev yes %.2f, %+.1f sd)", s.Hazard.Concern(), s.Probability, s.Z))
		result.RequiredChecks = append(result.RequiredChecks, s.Hazard.Check())
	}
	return result
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)`)

// patchedFunction reads the function the patch's first change inside a
// function lands in, from the repository as it stands at the base revision.
// A change outside every function, such as the import a remedy needs, is
// skipped: reviewed alone it named no function at all. The review judges the
// change against that source; when none can be found the review still reads
// the diff, which carries its own context lines.
func patchedFunction(repo, patch string) hotFunction {
	for _, c := range hunkChanges(patch) {
		if fn, _, err := functionAt(filepath.Join(repo, c.path), c.line); err == nil {
			return fn
		}
	}
	return hotFunction{}
}

type change struct {
	path string
	line int
}

// firstChange returns the old-side file and line of the patch's first removed
// or added line.
func firstChange(patch string) (string, int, bool) {
	changes := hunkChanges(patch)
	if len(changes) == 0 {
		return "", 0, false
	}
	return changes[0].path, changes[0].line, true
}

// hunkChanges returns, for every hunk, the old-side file and line of its first
// removed or added line.
func hunkChanges(patch string) []change {
	var out []change
	path, line, found := "", 0, false
	for _, text := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(text, "--- "):
			path = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(text, "--- ")), "a/")
		case hunkHeader.MatchString(text):
			line, _ = strconv.Atoi(hunkHeader.FindStringSubmatch(text)[1])
			found = false
		case found || line == 0:
		case strings.HasPrefix(text, " "):
			line++
		case strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+"):
			found = true
			if _, _, ok := parseLocation(path); ok {
				out = append(out, change{path, line})
			}
		}
	}
	return out
}
