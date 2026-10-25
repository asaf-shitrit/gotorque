package campaign

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/policy"
)

// TestRecordedVerdictsSurviveTheRobustShift is the ADR 0043 replay gate. It
// gives every measured candidate's wall-time comparisons the Hodges–Lehmann
// shift those samples produce and re-evaluates the verdict. The robust shift
// may only make an accept stricter: a new accept would be a false accept the
// mean refused to make, and a new rejection would be the A/A noise the
// regression side keeps the mean for.
//
// The checked-in campaigns under testdata/replay-campaigns keep the gate
// running in CI, including the held-out skip. When the campaigns recorded on
// this machine are present (they are absent in CI), they are replayed too and
// their tally is logged; the replay never reads a campaign on a held-out
// target (targets/HELDOUT.md), which is not where a change is tuned.
func TestRecordedVerdictsSurviveTheRobustShift(t *testing.T) {
	heldOut := heldOutRepositoryNames(t)
	fixture := replayCampaigns(t, filepath.Join("testdata", "replay-campaigns"), heldOut)
	require.Equal(t, 1, fixture.campaigns, "the fixture must replay a development-target campaign")
	require.Equal(t, 1, fixture.heldOut, "the fixture must exercise the held-out skip")
	require.Equal(t, 1, fixture.measured)
	require.Equal(t, 1, fixture.changed, "the fixture must exercise a verdict the shift changes")
	require.Equal(t, 1, fixture.stricter, "the fixture's change must be a stricter accept")
	fixture.requireNothingWorse(t)

	dir := recordedCampaignsDir()
	if _, err := os.Stat(dir); err != nil {
		t.Logf("recorded campaigns not available at %s, replayed the checked-in fixtures only: %v", dir, err)
		return
	}
	recorded := replayCampaigns(t, dir, heldOut)
	t.Logf("replayed %d measured candidates from %d campaigns (%d skipped as held-out, %d without a wall-time primary, %d without samples); verdicts changed: %d (%d stricter accepts, %d new accepts, %d new rejections); %d recorded verdicts already differed under the mean",
		recorded.measured, recorded.campaigns, recorded.heldOut, recorded.otherPrimary, recorded.noSamples, recorded.changed, recorded.stricter, recorded.newAccepts, recorded.newRejections, recorded.recordedDrift)
	recorded.requireNothingWorse(t)
	require.Positive(t, recorded.measured, "the recorded replay must judge something to be worth running")
}

type replayTally struct {
	campaigns, measured              int
	heldOut, otherPrimary, noSamples int
	changed, stricter                int
	newAccepts, newRejections        int
	recordedDrift                    int
	examples                         []string
}

func (tally replayTally) requireNothingWorse(t *testing.T) {
	t.Helper()
	require.Zero(t, tally.newAccepts, "the robust shift accepted a candidate the mean refused: %v", tally.examples)
	require.Zero(t, tally.newRejections, "the robust shift rejected a candidate the mean accepted: %v", tally.examples)
}

func replayCampaigns(t *testing.T, dir string, heldOut map[string]bool) replayTally {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var tally replayTally
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tally.oneCampaign(t, filepath.Join(dir, entry.Name(), "report.json"), heldOut)
	}
	return tally
}

func (tally *replayTally) oneCampaign(t *testing.T, path string, heldOut map[string]bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Logf("%s: not a readable campaign state: %v", path, err)
		return
	}
	repository := strings.ToLower(repoBasename(state.Manifest.Target.Repository))
	if repository == "" {
		return
	}
	if heldOut[repository] {
		tally.heldOut++
		return
	}
	tally.campaigns++
	config := policyConfigFromManifest(state.Manifest)
	if config.PrimaryMetric != "wall_time_ns" {
		tally.otherPrimary++
		return
	}
	for _, record := range state.CandidateRecords {
		tally.oneCandidate(t, path, config, record)
	}
}

func (tally *replayTally) oneCandidate(t *testing.T, campaign string, config policy.Config, record CandidateRecord) {
	t.Helper()
	if record.Unmeasured || len(record.Comparisons) == 0 {
		return
	}
	robust := make([]domain.MetricComparison, len(record.Comparisons))
	copy(robust, record.Comparisons)
	if !replayShifts(robust, record.Samples) {
		tally.noSamples++
		return
	}
	tally.measured++
	mean := policy.Evaluate(config, replayEvidence(config, record.Comparisons))
	shifted := policy.Evaluate(config, replayEvidence(config, robust))
	if mean.Decision != record.Decision {
		tally.recordedDrift++
	}
	if mean.Decision == shifted.Decision {
		return
	}
	tally.changed++
	tally.recordChange(mean.Decision, shifted.Decision)
	if len(tally.examples) < 20 {
		tally.examples = append(tally.examples, fmt.Sprintf("%s attempt %d: %s -> %s", filepath.Base(filepath.Dir(campaign)), record.Attempt, mean.Decision, shifted.Decision))
	}
}

func (tally *replayTally) recordChange(before, after domain.Decision) {
	switch {
	case after == domain.DecisionAccepted && before != domain.DecisionAccepted:
		tally.newAccepts++
	case after == domain.DecisionRejected && before != domain.DecisionRejected:
		tally.newRejections++
	case before == domain.DecisionAccepted:
		tally.stricter++
	}
}

// replayEvidence judges a recorded candidate's comparisons with the engine's
// eligibility rule, without the behavior and safety gates it already passed
// before these readings existed.
func replayEvidence(config policy.Config, comparisons []domain.MetricComparison) policy.Evidence {
	return policy.Evidence{
		BehaviorMatches:        true,
		SafetyChecksPassed:     true,
		RepresentativeEvidence: true,
		Comparisons:            comparisons,
		Primary:                eligibleReadings(config, comparisons),
	}
}

// replayShifts fills in each wall-time comparison's shift from the raw samples
// the record kept, and reports whether it measured at least one. The pooled
// reading is folded per repetition exactly as the evaluator builds it; a
// per-workload reading reads the sample row with that seed id.
func replayShifts(comparisons []domain.MetricComparison, samples []domain.WorkloadSamples) bool {
	measured := false
	for i := range comparisons {
		comparison := &comparisons[i]
		if comparison.Metric != "wall_time_ns" {
			continue
		}
		baseline, candidate, ok := replaySeries(comparison.Workload, samples)
		if !ok {
			continue
		}
		before := comparison.Shift
		shiftFromSamples(comparison, baseline, candidate)
		measured = measured || comparison.Shift != before
	}
	return measured
}

func replaySeries(workload string, samples []domain.WorkloadSamples) ([]float64, []float64, bool) {
	if len(samples) == 0 {
		return nil, nil, false
	}
	if workload != "" {
		for _, sample := range samples {
			if sample.Workload == workload {
				return sample.BaselineNs, sample.CandidateNs, true
			}
		}
		return nil, nil, false
	}
	baseline := make([][]float64, 0, len(samples))
	candidate := make([][]float64, 0, len(samples))
	for _, sample := range samples {
		baseline = append(baseline, sample.BaselineNs)
		candidate = append(candidate, sample.CandidateNs)
	}
	return meanPerRepetition(baseline), meanPerRepetition(candidate), true
}

// recordedCampaignsDir is where this machine's campaign runs live, overridable
// for a checkout elsewhere.
func recordedCampaignsDir() string {
	if dir := os.Getenv("GOTORQUE_REPLAY_CAMPAIGNS"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "projects", "gotorque-work", "campaigns")
}

// heldOutRepositoryNames maps the held-out target list to repository basenames,
// which is what a campaign's manifest carries: campaign manifests written from
// a local checkout record a path, not the target's own name.
func heldOutRepositoryNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, target := range heldOutTargets(t) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "targets", target, "manifest.json"))
		if err != nil {
			t.Logf("held-out target %q has no readable manifest: %v", target, err)
			continue
		}
		var manifest struct {
			Target struct {
				Repository string `json:"repository"`
			} `json:"target"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Logf("held-out target %q manifest does not parse: %v", target, err)
			continue
		}
		if name := repoBasename(manifest.Target.Repository); name != "" {
			names[name] = true
		}
	}
	return names
}

// heldOutTargets reads the target names out of the first column of
// targets/HELDOUT.md's table.
func heldOutTargets(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "targets", "HELDOUT.md"))
	require.NoError(t, err)
	var targets []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if ok {
			targets = append(targets, name)
		}
	}
	require.NotEmpty(t, targets, "targets/HELDOUT.md listed no held-out targets")
	return targets
}

// repoBasename is the repository's last path element, without a .git suffix:
// the local checkout directory for a campaign manifest, the repository name for
// a target manifest.
func repoBasename(repository string) string {
	repository = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(repository), "/"), ".git")
	if i := strings.LastIndexAny(repository, `/\`); i >= 0 {
		return repository[i+1:]
	}
	return repository
}
