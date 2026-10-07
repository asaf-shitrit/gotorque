package orchestrator

import (
	"context"
	"fmt"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
)

// fakeBench is the deterministic half of a campaign for graph tests: it
// discovers one fixed piece of evidence, assesses every proposal with a
// scripted verdict and remembers everything it was asked to settle. Variants
// are fields, not further fakes.
type fakeBench struct {
	// decisions is the verdict of each assessment in turn; the last repeats.
	// Empty means every candidate is rejected.
	decisions []domain.Decision
	// failureDetail is returned as CandidateEvidence.FailureDetail so tests
	// can verify rejection detail propagates into prior_candidates.
	failureDetail string
	// hotFunctions is what discovery measured.
	hotFunctions []string
	// unmeasuredFirst rejects attempt 1 before measurement.
	unmeasuredFirst bool

	discoverCalls int
	assessCalls   int
	// requests is every candidate request the graph handed to Assess.
	requests []CandidateRequest
	// settled is every settlement the graph handed to Settle, in order.
	settled []Settlement
	// promoted is the candidates Settle was handed with an accepting verdict.
	promoted []string

	// started and finished count the campaign-lifecycle notes; degraded and
	// repaired are the role notes, in order.
	started, finished int
	degraded          []degradedRole
	repaired          []repairedRole
}

// degradedRole is one absorbed role failure, as the graph reported it.
type degradedRole struct {
	role  string
	cause string
}

// repairedRole is one role output the decoder had to repair, as reported.
type repairedRole struct {
	role   string
	repair agents.Repair
}

func (b *fakeBench) Note(_ context.Context, n Note) error {
	switch n.Kind {
	case NoteStarted:
		b.started++
	case NoteFinished:
		b.finished++
	case NoteDegraded:
		b.degraded = append(b.degraded, degradedRole{role: n.Role, cause: n.Cause})
	case NoteRepaired:
		b.repaired = append(b.repaired, repairedRole{role: n.Role, repair: n.Repair})
	}
	return nil
}

func (b *fakeBench) Discover(context.Context, DiscoveryRequest) (DiscoveryEvidence, error) {
	b.discoverCalls++
	return DiscoveryEvidence{
		RunIDs:       []string{fmt.Sprintf("run-%d", b.discoverCalls)},
		HotFunctions: b.hotFunctions,
		Summary:      "measured parser hot path",
	}, nil
}

func (b *fakeBench) Assess(_ context.Context, req CandidateRequest) (Assessment, error) {
	b.assessCalls++
	b.requests = append(b.requests, req)
	id := fmt.Sprintf("candidate-%d", b.assessCalls)
	evidence := CandidateEvidence{
		Candidate: domain.Candidate{
			ID:           id,
			BaseRevision: req.Campaign.BaseRevision,
			Hypothesis:   req.Proposal.Hypothesis,
			PatchPath:    id + ".patch",
		},
		BehaviorMatches: true,
		Summary:         "candidate measured",
		FailureDetail:   b.failureDetail,
	}
	if b.unmeasuredFirst && req.Attempt == 1 {
		evidence.Unmeasured = true
		evidence.FailureDetail = "the patch uses bufio without importing it"
	}
	decision := domain.DecisionRejected
	if len(b.decisions) > 0 {
		decision = b.decisions[min(b.assessCalls-1, len(b.decisions)-1)]
	}
	return Assessment{
		Evidence: evidence,
		Verdict:  domain.Evaluation{CandidateID: id, Decision: decision, BehaviorMatches: true},
	}, nil
}

func (b *fakeBench) Settle(_ context.Context, s Settlement) error {
	b.settled = append(b.settled, s)
	if s.Assessment.Verdict.Decision == domain.DecisionAccepted {
		b.promoted = append(b.promoted, s.Assessment.Evidence.Candidate.ID)
	}
	return nil
}

// progress is the tallies each settlement carried.
func (b *fakeBench) progress() []CampaignProgress {
	out := make([]CampaignProgress, 0, len(b.settled))
	for _, s := range b.settled {
		out = append(out, s.Progress)
	}
	return out
}

// targets is the target each settlement was recorded with.
func (b *fakeBench) targets() []*agents.Target {
	out := make([]*agents.Target, 0, len(b.settled))
	for _, s := range b.settled {
		out = append(out, s.Target)
	}
	return out
}

// requestTargets is the target each assessment request carried.
func (b *fakeBench) requestTargets() []*agents.Target {
	out := make([]*agents.Target, 0, len(b.requests))
	for _, r := range b.requests {
		out = append(out, r.Target)
	}
	return out
}

// proposals is the optimizer output each assessment request carried.
func (b *fakeBench) proposals() []agents.OptimizerResult {
	out := make([]agents.OptimizerResult, 0, len(b.requests))
	for _, r := range b.requests {
		out = append(out, r.Proposal)
	}
	return out
}

// analyses is the analysis each assessment request carried.
func (b *fakeBench) analyses() []agents.AnalystResult {
	out := make([]agents.AnalystResult, 0, len(b.requests))
	for _, r := range b.requests {
		out = append(out, r.Analysis)
	}
	return out
}

// hotBench is a bench whose discovery measured one hot function.
func hotBench(decisions ...domain.Decision) *fakeBench {
	return &fakeBench{hotFunctions: []string{"main.go:207"}, decisions: decisions}
}
