package campaign

import (
	"context"
	"errors"
	"fmt"
	"os"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/policy"
)

// verifyAttemptOffset keeps a verification's patch file and candidate ID
// apart from the campaign's own attempts.
const verifyAttemptOffset = 9000

// DefaultVerifyPairs is the interleaved pair count a verification measures
// with: more than a campaign's 25, because a verification exists to catch the
// verdicts those 25 got wrong. Re-checking overnight-miller-1's false
// acceptance by hand took 60 pairs per order at low load to show the
// candidate within 0.4% of its baseline.
const DefaultVerifyPairs = 60

// Verification is one accepted candidate evaluated again from its recorded
// patch.
type Verification struct {
	Attempt       int                       `json:"attempt"`
	CandidateID   string                    `json:"candidate_id"`
	Pairs         int                       `json:"pairs"`
	Original      domain.Decision           `json:"original"`
	Decision      domain.Decision           `json:"decision"`
	Reasons       []string                  `json:"reasons,omitempty"`
	Comparisons   []domain.MetricComparison `json:"comparisons,omitempty"`
	LoadAverages  []float64                 `json:"load_averages,omitempty"`
	LoadContended bool                      `json:"load_contended,omitempty"`
	Summary       string                    `json:"summary,omitempty"`
}

// Confirmed reports whether the verification reached the same acceptance.
func (v Verification) Confirmed() bool {
	return v.Original == domain.DecisionAccepted && v.Decision == domain.DecisionAccepted
}

// Verify evaluates an earlier candidate again from its recorded patch,
// through the same evaluation and policy a campaign uses, with pairs
// interleaved A/B pairs per workload. The duplicate-patch and accepted-fix
// refusals are off for it: re-measuring a known patch is the point. The
// verification is persisted with the campaign.
func (e *Engine) Verify(ctx context.Context, attempt, pairs int) (Verification, error) {
	record, err := e.recordForAttempt(attempt)
	if err != nil {
		return Verification{}, err
	}
	patch, err := os.ReadFile(record.PatchPath)
	if err != nil {
		return Verification{}, fmt.Errorf("read attempt %d's patch: %w", attempt, err)
	}
	if err := e.runBuildStep(ctx); err != nil {
		return Verification{}, err
	}
	e.verifying, e.repetitions = true, pairs
	defer func() { e.verifying, e.repetitions = false, 0 }()
	evidence, err := e.evaluateCandidate(ctx, orchestrator.CandidateRequest{
		Campaign: e.campaignRequest(),
		Attempt:  verifyAttemptOffset + attempt,
		Proposal: agents.OptimizerResult{Patch: string(patch), Hypothesis: record.Hypothesis},
		Target:   record.Target,
	})
	if err != nil {
		return Verification{}, err
	}
	result := e.policyVerdict(evidence)
	v := Verification{
		Attempt: attempt, CandidateID: evidence.Candidate.ID, Pairs: pairs,
		Original: record.Decision, Decision: result.Decision, Reasons: result.Reasons, Comparisons: result.Comparisons,
		LoadAverages: evidence.LoadAverages, LoadContended: evidence.LoadContended, Summary: evidence.Summary,
	}
	e.state.Verifications = append(e.state.Verifications, v)
	return v, e.saveEvent("candidate_verified", fmt.Sprintf("attempt %d: %s on verification (originally %s)", attempt, v.Decision, v.Original), v)
}

// AcceptedAttempts lists the attempts the campaign accepted.
func (e *Engine) AcceptedAttempts() []int {
	var out []int
	for _, r := range e.state.CandidateRecords {
		if r.Accepted {
			out = append(out, r.Attempt)
		}
	}
	return out
}

func (e *Engine) recordForAttempt(attempt int) (CandidateRecord, error) {
	for _, r := range e.state.CandidateRecords {
		if r.Attempt != attempt {
			continue
		}
		if r.PatchPath == "" {
			return CandidateRecord{}, fmt.Errorf("attempt %d has no recorded patch", attempt)
		}
		return r, nil
	}
	return CandidateRecord{}, fmt.Errorf("campaign has no attempt %d", attempt)
}

// policyVerdict runs the campaign's acceptance policy over one candidate's
// evidence, exactly as the graph's decision node does.
func (e *Engine) policyVerdict(evidence orchestrator.CandidateEvidence) policy.Result {
	config := policyConfigFromManifest(e.state.Manifest)
	return policy.Evaluate(config, policy.Evidence{
		BehaviorMatches:        evidence.BehaviorMatches,
		FailureSummary:         evidence.Summary,
		SafetyChecksPassed:     evidence.SafetyChecksPassed,
		RepresentativeEvidence: evidence.RepresentativeEvidence,
		Comparisons:            evidence.Comparisons,
		Primary:                eligibleReadings(config, evidence.Comparisons),
	})
}

// pairs is the interleaved pair count per workload: the campaign's fixed
// count, or a verification's.
func (e *Engine) pairs() int {
	if e.repetitions > 0 {
		return e.repetitions
	}
	return measurementRepetitions
}

var ErrNothingAccepted = errors.New("the campaign accepted no candidate; pass --attempt to verify another")
