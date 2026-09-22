package campaign

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/profile"
	"example.com/gotorque/internal/runner"
	"example.com/gotorque/internal/workload"
)

// ExplorerJev marks a campaign whose extra discovery workloads were generated
// by code and judged by Jev rather than proposed by the explorer model.
const ExplorerJev = "jev"

const (
	// maxExploredWorkloads bounds the flag variants discovery samples next to
	// the seed. Each costs a sampling window, so three keeps discovery within
	// a quarter of a minute of what it cost before.
	maxExploredWorkloads = 3
	// maxHelpBytes bounds the help text sent as state.
	maxHelpBytes = 8 << 10
)

// exploredWorkload is one extra discovery workload: the sampled seed with one
// option added that Jev judged a processing mode and that changes the output.
type exploredWorkload struct {
	Seed manifest.SeedWorkload `json:"seed"`
	Flag string                `json:"flag"`
	Mode float64               `json:"mode"`
}

func (w exploredWorkload) String() string {
	return fmt.Sprintf("%s (Jev mode %.2f)", w.Seed.ID, w.Mode)
}

// exploreWorkloads replaces the explorer model with code and Jev. The model's
// proposals were validated and counted but never run, so discovery only ever
// saw the manifest's first seed, and a CLI's other modes stayed invisible:
// gron's --stream runs gronStream, a hot path the seed never reaches. Code
// finds the boolean options the target declares, Jev judges from the
// program's own help text which ones select a processing mode, and code
// keeps the variants that exit cleanly and change the output, since an option
// that changes nothing reaches no new code. Discovery samples each one.
func (e *Engine) exploreWorkloads(ctx context.Context, seed manifest.SeedWorkload) []exploredWorkload {
	evaluator := e.exploreEvaluator()
	if evaluator == nil {
		return nil
	}
	flags := e.untriedFlags(seed)
	if len(flags) == 0 {
		_ = e.saveEvent("workloads_explored", "no boolean options found to explore", nil)
		return nil
	}
	ranked, err := rankModes(ctx, evaluator, strings.Join(e.state.Manifest.Target.Command, " "), e.helpText(ctx, seed), flags)
	if err != nil {
		_ = e.saveEvent("workloads_explored", "Jev could not judge the options: "+err.Error(), nil)
		return nil
	}
	chosen := e.keepChangingVariants(ctx, seed, ranked)
	names := make([]string, 0, len(chosen))
	for _, w := range chosen {
		names = append(names, w.String())
	}
	e.state.DiscoveryWorkloads = names
	_ = e.saveEvent("workloads_explored", fmt.Sprintf("sampling %d option variant(s) of %s: %s", len(chosen), seed.ID, strings.Join(names, "; ")), chosen)
	return chosen
}

func (e *Engine) exploreEvaluator() jev.Evaluator {
	if e.adkAgents == nil {
		return nil
	}
	return e.adkAgents.ExploreEvaluator
}

// untriedFlags lists the target's boolean options the seed does not already
// pass. They are read from the build package, or, for a command whose flags
// live in a library package (gojq's are in ./cli), from the module package
// that declares the most.
func (e *Engine) untriedFlags(seed manifest.SeedWorkload) []workload.Flag {
	flags, _ := workload.BoolFlags(filepath.Join(e.state.Repository, e.state.Manifest.Target.Build.Package))
	if len(flags) == 0 {
		flags = richestFlagPackage(e.state.Repository)
	}
	var untried []workload.Flag
	for _, f := range flags {
		if !passes(seed.Args, f) {
			untried = append(untried, f)
		}
	}
	return untried
}

func passes(args []string, f workload.Flag) bool {
	for _, a := range args {
		if a == f.Name || strings.HasPrefix(a, f.Name+"=") {
			return true
		}
		for _, alias := range f.Aliases {
			if a == alias {
				return true
			}
		}
	}
	return false
}

func richestFlagPackage(repo string) []workload.Flag {
	var best []workload.Flag
	_ = filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir // an unreadable directory declares nothing we can read
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != repo && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
			return filepath.SkipDir
		}
		if flags, err := workload.BoolFlags(path); err == nil && len(flags) > len(best) {
			best = flags
		}
		return nil
	})
	return best
}

// sampleExplored samples each explored variant. One that cannot be sampled is
// recorded and left out; discovery still has the seed's sample.
func (e *Engine) sampleExplored(ctx context.Context, seed manifest.SeedWorkload) []profile.SampleResult {
	var results []profile.SampleResult
	for i, explored := range e.exploreWorkloads(ctx, seed) {
		result, err := e.sampleVariant(ctx, explored.Seed, fmt.Sprintf("sample-report-%d.txt", i+1))
		if err != nil {
			_ = e.saveEvent("workload_sample_skipped", fmt.Sprintf("%s could not be sampled: %v", explored.Seed.ID, err), nil)
			continue
		}
		results = append(results, result)
	}
	return results
}

// sampleVariant samples a variant on the seed's amplified input and, failing
// that, on the seed input repeated one copy per line. A mode can read its
// input differently from the default: gron --stream reads one document per
// line of at most 1 MiB, so on the 16 MiB single-line document the seed is
// sampled with it exits before the sampler attaches, and gronStream, the code
// the variant was chosen to reach, never showed in the profile. The default
// mode reads one document and ignores the rest, so the same line-repeated
// input would end it in 10 ms; neither shape serves both.
func (e *Engine) sampleVariant(ctx context.Context, seed manifest.SeedWorkload, reportName string) (profile.SampleResult, error) {
	result, err := e.sampleSeed(ctx, seed, reportName)
	if err == nil || seed.Stdin == "" {
		return result, err
	}
	result, lineErr := e.sampleWith(ctx, seed, repeatLines([]byte(seed.Stdin)), reportName)
	if lineErr != nil {
		return result, fmt.Errorf("%w; with one copy per line: %w", err, lineErr)
	}
	_ = e.saveEvent("workload_sample_input", fmt.Sprintf("%s sampled on its input repeated one copy per line; the amplified document failed: %v", seed.ID, err), nil)
	return result, nil
}

// variantRequest runs a workload on the release binary under the isolation
// discovery runs the seed with.
func (e *Engine) variantRequest(seed manifest.SeedWorkload) runner.RunRequest {
	req := e.seedMeasurementRequest(seed, e.state.BuildID, e.state.BinaryPath)
	req.NetworkAllowed = !e.state.LocalIsolation
	req.FilesystemAllowed = !e.state.LocalIsolation
	return req
}

// helpText runs the target with --help through the sandboxed runner and keeps
// what it printed on either stream, whatever the exit status: many CLIs print
// usage to stderr and exit 2.
func (e *Engine) helpText(ctx context.Context, seed manifest.SeedWorkload) string {
	req := e.variantRequest(seed)
	req.Workload.Command.Args = append(append([]string{}, e.state.Manifest.Target.Command...), "--help")
	req.Stdin = nil
	result, _ := e.runner.Run(ctx, req)
	var b strings.Builder
	for _, stream := range []string{"stdout", "stderr"} {
		if data, err := os.ReadFile(result.Artifacts[stream]); err == nil {
			b.Write(data)
		}
	}
	text := b.String()
	if len(text) > maxHelpBytes {
		text = text[:maxHelpBytes]
	}
	return text
}

type modeFlag struct {
	flag        workload.Flag
	probability float64
}

// rankModes asks about every option in one request and keeps those Jev judges
// a processing mode, most likely first; ties keep declaration order.
func rankModes(ctx context.Context, evaluator jev.Evaluator, command, help string, flags []workload.Flag) ([]modeFlag, error) {
	spelled := make(map[string][]string, len(flags))
	for _, f := range flags {
		spelled[f.Name] = f.Aliases
	}
	resp, err := evaluator.Evaluate(ctx, jev.Request{State: jev.FlagState(command, help), Questions: jev.FlagQuestions(spelled)})
	if err != nil {
		return nil, err
	}
	var ranked []modeFlag
	for _, f := range flags {
		if answer, ok := resp.Answers[f.Name]; ok && answer.Probability >= jev.ModeFloor {
			ranked = append(ranked, modeFlag{flag: f, probability: answer.Probability})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].probability > ranked[j].probability })
	return ranked, nil
}

// keepChangingVariants runs each candidate once, likeliest mode first, and keeps
// those that exit cleanly with output different from the seed's, up to the cap.
func (e *Engine) keepChangingVariants(ctx context.Context, seed manifest.SeedWorkload, ranked []modeFlag) []exploredWorkload {
	baseline, err := e.runner.Run(ctx, e.variantRequest(seed))
	if err != nil {
		return nil
	}
	var chosen []exploredWorkload
	for _, r := range ranked {
		if len(chosen) == maxExploredWorkloads {
			break
		}
		variant := seed
		variant.ID = seed.ID + " " + r.flag.Name
		// First, not last: the standard flag package stops at the first
		// positional argument, so an option after the seed's own arguments
		// would be read as one more of them.
		variant.Args = append([]string{r.flag.Name}, seed.Args...)
		result, err := e.runner.Run(ctx, e.variantRequest(variant))
		if err != nil || result.ExitCode != 0 || result.StdoutDigest == baseline.StdoutDigest {
			continue
		}
		chosen = append(chosen, exploredWorkload{Seed: variant, Flag: r.flag.Name, Mode: r.probability})
	}
	return chosen
}
