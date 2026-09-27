package campaign

import (
	"fmt"
	"path/filepath"

	"example.com/gotorque/internal/agents"
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
func loadHistory(dirs []string, revision string) ([]agents.Target, []HistorySource, error) {
	var targets []agents.Target
	sources := make([]HistorySource, 0, len(dirs))
	seen := map[string]bool{}
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve --history %s: %w", dir, err)
		}
		past, err := LoadReport(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("read --history %s: %w", dir, err)
		}
		source := HistorySource{Directory: abs, CampaignID: past.ID}
		if past.Environment.Revision != revision {
			source.Skipped = fmt.Sprintf("revision %s, not %s", shortRevision(past.Environment.Revision), shortRevision(revision))
			sources = append(sources, source)
			continue
		}
		for _, t := range measuredTargets(past.CandidateRecords) {
			key := t.Location + "\x00" + t.Cause
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, t)
			source.Targets++
		}
		sources = append(sources, source)
	}
	return targets, sources, nil
}

// measuredTargets are the targets of records whose candidate reached
// measurement: accepted, or compared against the baseline at all.
func measuredTargets(records []CandidateRecord) []agents.Target {
	var out []agents.Target
	for _, r := range records {
		if r.Target != nil && (r.Accepted || len(r.Comparisons) > 0) {
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
