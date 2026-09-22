package campaign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/profile"
	"github.com/stretchr/testify/require"
)

// optionsMain declares options of every kind the explorer must sort out: four
// modes that change the output, one that changes nothing, one that fails on
// this input, and a version flag Jev will not call a mode. It reads the file
// its first argument names, so an option placed after that argument would be
// ignored by the standard flag package.
const optionsMain = `package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	upper := flag.Bool("upper", false, "print in upper case")
	var reverse bool
	flag.BoolVar(&reverse, "reverse", false, "print reversed")
	flag.BoolVar(&reverse, "r", false, "")
	count := flag.Bool("count", false, "print the length")
	quote := flag.Bool("quote", false, "print quoted")
	quiet := flag.Bool("quiet", false, "accepted and ignored")
	strict := flag.Bool("strict", false, "fail on this input")
	version := flag.Bool("version", false, "print the version")
	flag.Parse()
	b, _ := os.ReadFile(flag.Arg(0))
	s := strings.TrimSpace(string(b))
	switch {
	case *version:
		fmt.Println("v1")
	case *strict:
		os.Exit(3)
	case *upper:
		s = strings.ToUpper(s)
	case reverse:
		s = s[len(s)-1:] + s[:len(s)-1]
	case *count:
		s = fmt.Sprint(len(s))
	case *quote:
		s = fmt.Sprintf("%q", s)
	}
	_ = quiet
	fmt.Println(s)
}
`

func optionsEngine(t *testing.T, evaluator jev.Evaluator) *Engine {
	t.Helper()
	repo := makeRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte(optionsMain), 0o600))
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qam", "options")
	engine, err := Create(context.Background(), Options{Repository: repo, ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	require.NoError(t, engine.Run(context.Background()))
	engine.SetADK(&agents.Set{ExploreEvaluator: evaluator}, nil)
	return engine
}

// modeEvaluator answers each option question with a scripted probability and
// remembers the state it was shown.
type modeEvaluator struct {
	modes map[string]float64
	state map[string]string
	err   error
}

func (m *modeEvaluator) Evaluate(_ context.Context, req jev.Request) (jev.Response, error) {
	m.state, _ = req.State.(map[string]string)
	answers := map[string]jev.Answer{}
	for id := range req.Questions {
		answers[id] = jev.Answer{Type: "boolean", Probability: m.modes[id]}
	}
	return jev.Response{Answers: answers}, m.err
}

func TestExploreKeepsTheLikeliestModesThatChangeTheOutput(t *testing.T) {
	evaluator := &modeEvaluator{modes: map[string]float64{"--strict": 0.99, "--upper": 0.97, "--quiet": 0.95, "--reverse": 0.9, "--count": 0.8, "--quote": 0.7, "--version": 0.1}}
	engine := optionsEngine(t, evaluator)
	seed := engine.state.Manifest.Workloads.Seeds[0]
	seed.Args = []string{"fixture.txt"}

	chosen := engine.exploreWorkloads(context.Background(), seed)
	flags := make([]string, 0, len(chosen))
	for _, w := range chosen {
		flags = append(flags, w.Flag)
	}
	// --strict fails and --quiet changes nothing; the cap stops before --quote.
	require.Equal(t, []string{"--upper", "--reverse", "--count"}, flags)
	require.Equal(t, []string{"--upper", "fixture.txt"}, chosen[0].Seed.Args)
	require.Equal(t, "fixture --upper (Jev mode 0.97)", chosen[0].String())
	require.Len(t, engine.state.DiscoveryWorkloads, 3)
	require.Contains(t, RenderMarkdown(engine.State()), "- Explorer: the target's own options, judged by Jev (`typesafe-ai/jev`); discovery also sampled: fixture --upper (Jev mode 0.97); fixture --reverse (Jev mode 0.90); fixture --count (Jev mode 0.80)")
	require.Contains(t, evaluator.state["help"], "print in upper case", "the help text comes from the target's own --help")
}

func TestExploreDoesNothingWithoutJev(t *testing.T) {
	engine := optionsEngine(t, nil)
	engine.adkAgents = nil
	require.Nil(t, engine.exploreWorkloads(context.Background(), engine.state.Manifest.Workloads.Seeds[0]))
}

func TestExploreRecordsWhyItChoseNothing(t *testing.T) {
	engine := optionsEngine(t, &modeEvaluator{err: errors.New("gateway returned HTTP 429 for Jev")})
	require.Nil(t, engine.exploreWorkloads(context.Background(), engine.state.Manifest.Workloads.Seeds[0]))
	events, err := engine.store.Events()
	require.NoError(t, err)
	require.Contains(t, events[len(events)-1].Message, "Jev could not judge the options")

	seed := manifest.SeedWorkload{ID: "all-flags", Args: []string{"--upper", "-r", "--count", "--quote", "--quiet", "--strict", "--version"}}
	require.Nil(t, engine.exploreWorkloads(context.Background(), seed))
}

// TestFlagsAreFoundWhereTheCommandKeepsThem: gojq's command package declares
// no flags; its cli package does.
func TestFlagsAreFoundWhereTheCommandKeepsThem(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cmd", "tool"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cli"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cmd", "tool", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cli", "cli.go"), []byte("package cli\n\ntype opts struct {\n\tSlurp bool `long:\"slurp\" short:\"s\"`\n}\n"), 0o600))
	engine := &Engine{}
	engine.state.Repository = repo
	engine.state.Manifest.Target.Build.Package = "./cmd/tool"
	flags := engine.untriedFlags(manifest.SeedWorkload{Args: []string{"-x"}})
	require.Len(t, flags, 1)
	require.Equal(t, "--slurp", flags[0].Name)
	require.Empty(t, engine.untriedFlags(manifest.SeedWorkload{Args: []string{"-s"}}))
}

// TestMergedSamplesWeighEachWorkloadEqually: a mode only one variant reaches
// ranks by its share of that variant's time, not behind the seed's totals.
func TestMergedSamplesWeighEachWorkloadEqually(t *testing.T) {
	seedSample := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.sort"}, Weight: 900}, {Frames: []string{"main.print"}, Weight: 100}}}
	variantSample := profile.SampleResult{Stacks: []profile.Stack{{Frames: []string{"main.stream"}, Weight: 40}}}
	merged := mergeAttributed([]profile.SampleResult{seedSample, variantSample}, func(s string) bool { return strings.HasPrefix(s, "main.") })
	names := make([]string, 0, len(merged))
	for _, fn := range merged {
		names = append(names, fn.Name)
	}
	require.Equal(t, []string{"main.stream", "main.sort", "main.print"}, names)
}

// TestLineRepetitionFeedsALineOrientedMode: gron --stream reads one document per
// line, so the fallback input is the seed, newline-terminated, many times over.
func TestLineRepetitionFeedsALineOrientedMode(t *testing.T) {
	seed := []byte(`{"users":[1,2]}` + "\n")
	amplified := repeatLines(seed[:len(seed)-1])
	require.GreaterOrEqual(t, len(amplified), amplificationTarget)
	lines := strings.Split(strings.TrimSuffix(string(amplified), "\n"), "\n")
	require.Greater(t, len(lines), 1)
	for _, line := range lines {
		require.JSONEq(t, `{"users":[1,2]}`, line)
	}
	require.Equal(t, amplified, repeatLines(seed), "a trailing newline is not doubled")
}
