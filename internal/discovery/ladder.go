package discovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

// Event is something discovery wants the campaign to record. Discovery never
// writes to the campaign's store itself: the events travel in Evidence.
type Event struct {
	Kind    string
	Message string
	Data    any
}

// Ladder is the fallback ladder that turns a seed workload into a sample.
// Each rung answers one way a sample was seen to fail:
//
//   - the sampler recorded nothing: profile.Sample tries the same input once more;
//   - the target ended before the sampler could attach: the same seed again on
//     an input eight times larger, when its inputs can grow (retriesLarger);
//   - that still fails: the next stress-tier seed (FirstLiving);
//   - a variant that reads its input differently: the seed's input repeated
//     one copy per line (Variants).
//
// Past the last rung the caller falls back to the module's benchmarks.
type Ladder struct {
	in      Inputs
	sampler profile.Sampler
	notes   []string
	events  []Event
}

// NewLadder returns a ladder that samples the workloads of in with sampler.
func NewLadder(in Inputs, sampler profile.Sampler) *Ladder {
	return &Ladder{in: in, sampler: sampler}
}

// Notes returns the isolation notes the sampled runs collected, which the
// campaign folds into its deduplicated set.
func (l *Ladder) Notes() []string { return l.notes }

// TakeEvents returns the events recorded since the last call.
func (l *Ladder) TakeEvents() []Event {
	events := l.events
	l.events = nil
	return events
}

func (l *Ladder) event(kind, message string, data any) {
	l.events = append(l.events, Event{Kind: kind, Message: message, Data: data})
}

// FirstLiving samples the first seed, and when that fails, each stress-tier
// seed in manifest order, returning the first that sampled.
//
// Amplification only grows stdin, so a seed whose input is files runs as long
// as its files make it. go-jsonnet evaluates a .jsonnet file in 90 ms, and the
// macOS sampler cannot attach to a process that short: every attempt, even with
// sample -wait, wrote an empty call graph. Discovery then fell back to the
// module's benchmarks, which exercise unrelated code, and no target was chosen.
// A stress seed is the manifest's own larger version of a workload, so it can
// run long enough to sample.
func (l *Ladder) FirstLiving(ctx context.Context) (manifest.SeedWorkload, profile.SampleResult, error) {
	seeds := l.in.Seeds
	candidates := []manifest.SeedWorkload{seeds[0]}
	for _, seed := range seeds[1:] {
		if seed.Tier == domain.TierStress {
			candidates = append(candidates, seed)
		}
	}
	var failures []string
	for _, seed := range candidates {
		result, err := l.seed(ctx, seed, "sample-report.txt")
		if err == nil {
			return seed, result, nil
		}
		failures = append(failures, seed.ID+": "+err.Error())
	}
	return manifest.SeedWorkload{}, profile.SampleResult{}, errors.New(strings.Join(failures, "; "))
}

// Variants samples each explored variant. One that cannot be sampled is
// recorded and left out; discovery still has the seed's sample.
func (l *Ladder) Variants(ctx context.Context, variants []manifest.SeedWorkload) []profile.SampleResult {
	var results []profile.SampleResult
	for i, variant := range variants {
		result, err := l.variant(ctx, variant, fmt.Sprintf("sample-report-%d.txt", i+1))
		if err != nil {
			l.event("workload_sample_skipped", fmt.Sprintf("%s could not be sampled: %v", variant.ID, err), nil)
			continue
		}
		results = append(results, result)
	}
	return results
}

// seed samples one workload under the platform sampler, with its input
// amplified so the target outlives the sampling window.
func (l *Ladder) seed(ctx context.Context, seed manifest.SeedWorkload, reportName string) (profile.SampleResult, error) {
	result, err := l.amplified(ctx, seed, AmplificationTarget, reportName)
	// A fixed size cannot fit every CLI: 16 MiB of CSV kept held-out csvq
	// busy for 0.48s, just short of the sampler's half-second. A target that
	// exited that early, on inputs the manifest declares repeatable, gets one
	// more try at eight times the size. Ending just after the liveness check
	// fails differently: the sampler attaches and records an empty call graph
	// (held-out csvq again, sampled 0.58s after launch), so that counts too.
	if RetriesLarger(err, seed) {
		result, err = l.amplified(ctx, seed, RetryAmplificationTarget, reportName)
	}
	return result, err
}

func (l *Ladder) amplified(ctx context.Context, seed manifest.SeedWorkload, target int, reportName string) (profile.SampleResult, error) {
	amplified := AmplifyRepeats(seed, target)
	stdin := amplified.StdinBytes()
	if seed.StdinRepeat == 0 {
		stdin = AmplifyStdin(stdin)
	}
	return l.sampleWith(ctx, amplified, stdin, reportName)
}

// variant samples a variant on the seed's amplified input and, failing
// that, on the seed input repeated one copy per line. A mode can read its
// input differently from the default: gron --stream reads one document per
// line of at most 1 MiB, so on the 16 MiB single-line document the seed is
// sampled with it exits before the sampler attaches, and gronStream, the code
// the variant was chosen to reach, never showed in the profile. The default
// mode reads one document and ignores the rest, so the same line-repeated
// input would end it in 10 ms; neither shape serves both.
func (l *Ladder) variant(ctx context.Context, seed manifest.SeedWorkload, reportName string) (profile.SampleResult, error) {
	result, err := l.seed(ctx, seed, reportName)
	if err == nil || seed.Stdin == "" {
		return result, err
	}
	result, lineErr := l.sampleWith(ctx, seed, RepeatLines(seed.StdinBytes()), reportName)
	if lineErr != nil {
		return result, fmt.Errorf("%w; with one copy per line: %w", err, lineErr)
	}
	l.event("workload_sample_input", fmt.Sprintf("%s sampled on its input repeated one copy per line; the amplified document failed: %v", seed.ID, err), nil)
	return result, nil
}

// sampleWith samples one workload on the given input.
func (l *Ladder) sampleWith(ctx context.Context, seed manifest.SeedWorkload, stdin []byte, reportName string) (profile.SampleResult, error) {
	result, err := profile.Sample(ctx, l.sampler, profile.SampleTarget{
		BinaryPath: l.in.BinaryPath,
		Args:       append(append([]string{}, l.in.Command...), seed.Args...),
		Stdin:      stdin,
		Fixtures:   seed.Fixtures(),
		Duration:   4 * time.Second,
		OutputPath: filepath.Join(l.in.Dir, "profile-sample", reportName),
		Sandbox:    l.in.Sandbox,
	})
	l.notes = append(l.notes, result.IsolationNotes...)
	return result, err
}
