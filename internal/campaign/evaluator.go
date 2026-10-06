package campaign

import (
	"context"
	"fmt"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/runner"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// evaluator is candidate evaluation as a module (ADR 0036): it takes a
// proposal and returns the evidence for it, building, test-gating and
// measuring on its own. It holds no *Engine. Everything it needs is copied in
// when it is built, once per evaluation from the campaign's current state
// (the records, the baseline's passes and the discovery profile change between
// calls), and everything it must tell the campaign goes out through a port:
// the journal, the machine, and the test baseline. Toolchain and runner are
// concrete, since a real Git and a real runner are what tests use too.
//
// The settings that differ between a campaign attempt, a verification and a
// null candidate are an argument to evaluate, not state of the evaluator.
type evaluator struct {
	// dir is the campaign directory: patches, builds, benchstat samples and
	// worktrees live under it.
	dir          string
	campaignID   string
	manifest     manifest.Manifest
	repository   string
	baseRevision string
	buildID      string
	binaryPath   string
	// pgoProfilePath is the discovery CPU profile the PGO lane builds with.
	pgoProfilePath  string
	localIsolation  bool
	pgoBuildTimeout time.Duration
	toolchain       *toolchain.Toolchain
	runner          *runner.Runner
	journal         journal
	machine         machine
	baseline        testBaseline
	// now and elapsed are the wall clock and the campaign's spent run time,
	// for the PGO lane's budget guard.
	now     func() time.Time
	elapsed func() time.Duration
}

// newEvaluator builds an evaluator from the campaign's current state, with the
// ports the settings and the engine select.
func (e *Engine) newEvaluator(s evalSettings) *evaluator {
	return &evaluator{
		dir:             e.dir,
		campaignID:      e.state.ID,
		manifest:        e.state.Manifest,
		repository:      e.state.Repository,
		baseRevision:    e.state.Environment.Revision,
		buildID:         e.state.BuildID,
		binaryPath:      e.state.BinaryPath,
		pgoProfilePath:  e.state.PGOProfilePath,
		localIsolation:  e.state.LocalIsolation,
		pgoBuildTimeout: e.pgoBuildTimeout,
		toolchain:       e.toolchain,
		runner:          e.runner,
		journal:         e.evalJournal(),
		machine:         e.evalMachine(),
		baseline:        e.evalBaseline(s),
		now:             e.now,
		elapsed:         func() time.Duration { return e.elapsedRunTime(e.now()) },
	}
}

// evaluateCandidate is a campaign attempt's evaluation; it is invoked through
// the orchestrator CandidateService adapter.
func (e *Engine) evaluateCandidate(ctx context.Context, req orchestrator.CandidateRequest) (orchestrator.CandidateEvidence, error) {
	return e.evaluateWith(ctx, req, e.campaignSettings())
}

// evaluateWith evaluates one proposal under the given settings.
func (e *Engine) evaluateWith(ctx context.Context, req orchestrator.CandidateRequest, s evalSettings) (orchestrator.CandidateEvidence, error) {
	return e.newEvaluator(s).evaluate(ctx, req, s)
}

// campaignSettings are the settings of a campaign's own attempts: its pair
// count, the refusals of what it already knows, the PGO lane, and its own
// test baseline.
func (e *Engine) campaignSettings() evalSettings {
	return evalSettings{pairs: measurementRepetitions, refuseRepeats: true, pgoLane: true, known: e.knownCandidates()}
}

// baseSuite is the unpatched revision's test suite, for the campaign's
// baseline step.
func (e *Engine) baseSuite() baseSuite {
	return baseSuite{toolchain: e.toolchain, dir: e.dir, repository: e.state.Repository, revision: e.state.Environment.Revision}
}

func (ev *evaluator) baseSuite() baseSuite {
	return baseSuite{toolchain: ev.toolchain, dir: ev.dir, repository: ev.repository, revision: ev.baseRevision}
}

// knownCandidates is what a campaign, and the --history campaigns it counts as
// tried, already learned about candidates: where each measured one was
// measured, and which functions an accepted fix already sped up (ADR 0031).
type knownCandidates struct {
	measured map[string]string
	fixes    []AcceptedFix
}

// knownCandidates reads them from the campaign's current state.
func (e *Engine) knownCandidates() knownCandidates {
	known := knownCandidates{measured: map[string]string{}, fixes: append([]AcceptedFix(nil), e.state.HistoryAccepted...)}
	for id, where := range e.state.HistoryCandidates {
		known.measured[id] = where
	}
	for _, r := range e.state.CandidateRecords {
		if _, seen := known.measured[r.CandidateID]; !seen && isMeasured(r) {
			known.measured[r.CandidateID] = fmt.Sprintf("attempt %d of this campaign: %s", r.Attempt, r.Decision)
		}
		if r.Accepted && r.Target != nil {
			known.fixes = append(known.fixes, AcceptedFix{Function: r.Target.Function, Location: r.Target.Location, Where: fmt.Sprintf("attempt %d of this campaign", r.Attempt)})
		}
	}
	return known
}

// measuredAt reports where candidate id was already measured.
func (k knownCandidates) measuredAt(id string) (string, bool) {
	where, ok := k.measured[id]
	return where, ok
}
