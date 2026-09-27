package campaign

import (
	"fmt"
	"path/filepath"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/orchestrator"
)

// HistorySource is one earlier campaign --history read, and what it
// contributed. Skipped campaigns are kept too, with the reason, so a report
// reader can tell "nothing to carry" from "wrong revision".
type HistorySource struct {
	Directory  string `json:"directory"`
	CampaignID string `json:"campaign_id,omitempty"`
	Targets    int    `json:"targets"`
	Skipped    string `json:"skipped,omitempty"`
}

// loadHistory reads earlier campaigns of the same repository revision and
// returns every target one of their candidates reached measurement on. The
// engine counts those targets as already tried (priorTargets), so a new
// campaign spends its attempts on targets nothing has measured yet instead of
// re-proposing one an earlier campaign measured at no gain: dasel's
// WithExecutorID string-building fix came back inconclusive in three
// consecutive campaigns this way.
//
// A target from another revision is skipped rather than trusted: its location
// may point at different code. An unmeasured candidate (rejected before
// measurement) says nothing about its target, so it is not carried either.
// This is code-side bookkeeping only; no agent sees or decides it.
func loadHistory(dirs []string, revision string) (History, error) {
	h := History{Candidates: map[string]string{}}
	seen := map[string]bool{}
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return History{}, fmt.Errorf("resolve --history %s: %w", dir, err)
		}
		past, err := LoadReport(abs)
		if err != nil {
			return History{}, fmt.Errorf("read --history %s: %w", dir, err)
		}
		source := HistorySource{Directory: abs, CampaignID: past.ID}
		if past.Environment.Revision != revision {
			source.Skipped = fmt.Sprintf("revision %s, not %s", shortRevision(past.Environment.Revision), shortRevision(revision))
			h.Sources = append(h.Sources, source)
			continue
		}
		for _, t := range measuredTargets(past.CandidateRecords) {
			key := t.Location + "\x00" + t.Cause
			if seen[key] {
				continue
			}
			seen[key] = true
			h.Targets = append(h.Targets, t)
			source.Targets++
		}
		addMeasuredCandidates(h.Candidates, past)
		h.Priors = appendEarlierCandidates(h.Priors, past.CandidateRecords)
		h.Sources = append(h.Sources, source)
	}
	return h, nil
}

// History is what --history carries into a new campaign: the targets earlier
// campaigns measured, the IDs of the candidates they measured (a digest of
// revision and patch, so an equal ID is the same patch), each mapped to where
// and how it was judged, and the sources read.
type History struct {
	Targets    []agents.Target
	Candidates map[string]string
	Sources    []HistorySource
	// Priors are the measured candidates themselves, most recent campaign
	// first, capped at maxEarlierCandidates, for the optimizer to read.
	Priors []orchestrator.PriorCandidate
}

// maxEarlierCandidates bounds how much history reaches the optimizer's
// prompt; the duplicate check does not depend on it.
const maxEarlierCandidates = 16

func addMeasuredCandidates(into map[string]string, past State) {
	for _, r := range past.CandidateRecords {
		if r.CandidateID == "" || !isMeasured(r) {
			continue
		}
		if _, ok := into[r.CandidateID]; !ok {
			into[r.CandidateID] = fmt.Sprintf("campaign %s attempt %d: %s", past.ID, r.Attempt, r.Decision)
		}
	}
}

// appendEarlierCandidates adds past's measured candidates, as the optimizer
// reads them, until maxEarlierCandidates is reached.
func appendEarlierCandidates(into []orchestrator.PriorCandidate, records []CandidateRecord) []orchestrator.PriorCandidate {
	for _, r := range records {
		if len(into) == maxEarlierCandidates {
			break
		}
		if !isMeasured(r) {
			continue
		}
		into = append(into, orchestrator.PriorCandidate{Hypothesis: r.Hypothesis, Decision: string(r.Decision), Reasons: r.Reasons, Target: r.Target})
	}
	return into
}

func isMeasured(r CandidateRecord) bool { return r.Accepted || len(r.Comparisons) > 0 }

// measuredTargets are the targets of records whose candidate reached
// measurement: accepted, or compared against the baseline at all.
func measuredTargets(records []CandidateRecord) []agents.Target {
	var out []agents.Target
	for _, r := range records {
		if r.Target != nil && isMeasured(r) {
			out = append(out, *r.Target)
		}
	}
	return out
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	if revision == "" {
		return "unknown"
	}
	return revision
}
