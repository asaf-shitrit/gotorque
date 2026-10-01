package campaign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
	"github.com/asaf-shitrit/gotorque/internal/policy"
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
	// OutputChecks names the input variants the candidate and baseline were
	// both run on, and OutputMismatches the ones whose exit code or stdout
	// differed (verifyOutputs).
	OutputChecks     []string `json:"output_checks,omitempty"`
	OutputMismatches []string `json:"output_mismatches,omitempty"`
}

// Confirmed reports whether the verification reached the same acceptance.
func (v Verification) Confirmed() bool {
	return v.Original == domain.DecisionAccepted && v.Decision == domain.DecisionAccepted && len(v.OutputMismatches) == 0
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
	if binary := candidateBinaryOf(evidence); binary != "" && v.Decision == domain.DecisionAccepted {
		v.OutputChecks, v.OutputMismatches = e.verifyOutputs(ctx, binary, evidence.Candidate.ID)
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

// candidateBinaryOf is the candidate binary evaluateCandidate built, or ""
// when it never got that far.
func candidateBinaryOf(evidence orchestrator.CandidateEvidence) string {
	for _, uri := range evidence.ArtifactURIs {
		if filepath.Base(filepath.Dir(uri)) == "builds" {
			return uri
		}
	}
	return ""
}

// verifyOutputs runs the baseline and the candidate on inputs the manifest
// never tried and compares their exit codes and stdout. A candidate is
// measured on its seed workloads, and the behavior gate holds it to the
// target's own tests, but an optimization can still change output only on an
// input neither exercises: empty, cut short, or longer than any seed. For
// every representative seed, each variant of its input (see inputVariants)
// is run once on each binary; a variant the baseline itself does not answer
// deterministically is skipped rather than compared.
func (e *Engine) verifyOutputs(ctx context.Context, candidateBinary, candidateID string) (checks, mismatches []string) {
	for _, seed := range e.state.Manifest.Workloads.Seeds {
		if seed.Tier != domain.TierRepresentative {
			continue
		}
		for name, variant := range inputVariants(seed) {
			label := seed.ID + "/" + name
			base := e.seedMeasurementRequest(variant, e.state.BuildID, e.state.BinaryPath)
			if !e.outputIsDeterministic(ctx, base) {
				continue
			}
			cand := e.seedMeasurementRequest(variant, candidateID, candidateBinary)
			baseRun, baseErr := e.runner.Run(ctx, base)
			candRun, candErr := e.runner.Run(ctx, cand)
			checks = append(checks, label)
			if (baseErr == nil) != (candErr == nil) || baseRun.ExitCode != candRun.ExitCode || baseRun.StdoutDigest != candRun.StdoutDigest {
				mismatches = append(mismatches, label)
			}
		}
	}
	sort.Strings(checks)
	sort.Strings(mismatches)
	return checks, mismatches
}

// inputVariants derives, from a seed's input, the variants verifyOutputs
// runs: the same workload with its input empty, cut to its first half at a
// line boundary, and doubled. The input varied is stdin when the seed has
// one, else its largest fixture file; a seed with neither has no variants.
func inputVariants(seed manifest.SeedWorkload) map[string]manifest.SeedWorkload {
	stdin := seed.StdinBytes()
	if len(stdin) > 0 {
		out := map[string]manifest.SeedWorkload{}
		for name, data := range variantBytes(stdin) {
			v := seed
			v.Stdin, v.StdinHeader, v.StdinRepeat = string(data), "", 0
			out[name] = v
		}
		return out
	}
	i := largestFixture(seed.Files)
	if i < 0 {
		return nil
	}
	out := map[string]manifest.SeedWorkload{}
	for name, data := range variantBytes(seed.Files[i].Bytes()) {
		v := seed
		v.Files = append([]manifest.FixtureFile(nil), seed.Files...)
		v.Files[i] = manifest.FixtureFile{Path: seed.Files[i].Path, Content: string(data)}
		out[name] = v
	}
	return out
}

func variantBytes(data []byte) map[string][]byte {
	half := data[:len(data)/2]
	if cut := bytes.LastIndexByte(half, '\n'); cut >= 0 {
		half = half[:cut+1]
	}
	return map[string][]byte{
		"empty":   {},
		"half":    half,
		"doubled": append(append([]byte{}, data...), data...),
	}
}

func largestFixture(files []manifest.FixtureFile) int {
	best, size := -1, -1
	for i, f := range files {
		if n := len(f.Bytes()); n > size {
			best, size = i, n
		}
	}
	return best
}
