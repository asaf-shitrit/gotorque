package campaign

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

const causeFixture = `package fixture

import (
	"fmt"
	"io"
)

var table = map[string]int{}

// write prints every record on its own write call.
func write(w io.Writer, records []string) {
	for _, r := range records {
		fmt.Fprintln(w, r)
	}
}

type box[T any] struct{ v T }

func (b *box[T]) get() T { return b.v }

type pair[K comparable, V any] struct{}

func (pair[K, V]) plain() int {
	return 1
}
`

func writeCauseFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fixture.go"), []byte(causeFixture), 0o600))
	return repo
}

func TestFunctionAtReturnsTheWholeDeclarationWithItsDoc(t *testing.T) {
	file := filepath.Join(writeCauseFixture(t), "fixture.go")
	fn, start, err := functionAt(file, 14)
	require.NoError(t, err)
	require.Equal(t, "write", fn.Name)
	require.Equal(t, 11, start)
	require.True(t, strings.HasPrefix(fn.Source, "// write prints every record"), fn.Source)
	require.True(t, strings.HasSuffix(fn.Source, "}"), fn.Source)

	generic, _, err := functionAt(file, 19)
	require.NoError(t, err)
	require.Equal(t, "(*box).get", generic.Name)
	twoParams, _, err := functionAt(file, 24)
	require.NoError(t, err)
	require.Equal(t, "(pair).plain", twoParams.Name)
}

func TestFunctionAtExplainsWhatItCannotClassify(t *testing.T) {
	repo := writeCauseFixture(t)
	_, _, err := functionAt(filepath.Join(repo, "fixture.go"), 8)
	require.ErrorContains(t, err, "no function declaration contains line 8")
	_, _, err = functionAt(filepath.Join(repo, "missing.go"), 1)
	require.Error(t, err)

	broken := filepath.Join(repo, "broken.go")
	require.NoError(t, os.WriteFile(broken, []byte("package fixture\nfunc {"), 0o600))
	_, _, err = functionAt(broken, 2)
	require.Error(t, err)

	large := filepath.Join(repo, "large.go")
	require.NoError(t, os.WriteFile(large, []byte("package fixture\nfunc big() {\n"+strings.Repeat("\t_ = 1\n", maxCauseSourceBytes/6)+"}\n"), 0o600))
	_, _, err = functionAt(large, 3)
	require.ErrorContains(t, err, "classification limit")
}

func TestHotFunctionsResolvesEachFunctionOnceInHotnessOrder(t *testing.T) {
	repo := writeCauseFixture(t)
	sites, skipped := hotFunctions(repo, []string{
		"runtime.mallocgc",  // no position: skipped silently, as excerpts do
		"fixture.go:14",     // write
		"fixture.go:13",     // write again: one site per function
		"/abs/fixture.go:3", // absolute: refused by parseLocation
		"notes.txt:2",       // not Go source
		"fixture.go:8",      // package-level var: reported
		"fixture.go:19",     // get
	})
	require.Len(t, sites, 2)
	require.Equal(t, "fixture.go:14", sites[0].Location)
	require.Equal(t, "write", sites[0].Name)
	require.Equal(t, "fixture.go", sites[0].Path)
	require.Equal(t, "(*box).get", sites[1].Name)
	require.Len(t, skipped, 1)
	require.Contains(t, skipped[0], "fixture.go:8: not classified")
}

func TestHotFunctionsStopsAtTheSiteBudget(t *testing.T) {
	repo := t.TempDir()
	var src strings.Builder
	src.WriteString("package fixture\n")
	locations := make([]string, 0, maxCauseSites+3)
	for i := range maxCauseSites + 3 {
		fmt.Fprintf(&src, "func f%d() {}\n", i)
		locations = append(locations, fmt.Sprintf("many.go:%d", i+2))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "many.go"), []byte(src.String()), 0o600))
	sites, _ := hotFunctions(repo, locations)
	require.Len(t, sites, maxCauseSites)
}

// scriptedEvaluator answers by function name: a probability per cause, or an
// error. Unscripted answers are 0.05, below Jev's usual answer to every
// question, so only scripted causes can stand out.
type scriptedEvaluator struct {
	answers map[string]map[jev.Cause]float64
	fail    map[string]error
	partial bool
	calls   int
}

func (s *scriptedEvaluator) Evaluate(_ context.Context, req jev.Request) (jev.Response, error) {
	s.calls++
	source := req.State.(map[string]string)["source"]
	for name, err := range s.fail {
		if strings.Contains(source, "func "+name) || strings.Contains(source, ") "+name) {
			return jev.Response{}, err
		}
	}
	answers := map[string]jev.Answer{}
	for _, cause := range jev.Causes {
		answers[string(cause)] = jev.Answer{Type: "boolean", Probability: 0.05}
	}
	for name, script := range s.answers {
		if strings.Contains(source, "func "+name) || strings.Contains(source, ") "+name) {
			for cause, p := range script {
				answers[string(cause)] = jev.Answer{Type: "boolean", Probability: p}
			}
		}
	}
	if s.partial {
		delete(answers, string(jev.CauseRedundant))
	}
	return jev.Response{Model: jev.Model, Answers: answers, Usage: jev.Usage{InputTokens: 900, OutputTokens: 70}}, nil
}

func causeRequest(repo string, locations ...string) orchestrator.CauseRequest {
	return orchestrator.CauseRequest{Campaign: orchestrator.CampaignRequest{Repository: repo}, Discovery: orchestrator.DiscoveryEvidence{HotFunctions: locations}}
}

func TestCauseAnalystTurnsRankedAnswersIntoAnalysis(t *testing.T) {
	repo := writeCauseFixture(t)
	evaluator := &scriptedEvaluator{answers: map[string]map[jev.Cause]float64{
		"write": {jev.CauseUnbufferedIO: 0.9, jev.CauseAlloc: 0.95},
		"get":   {jev.CauseRedundant: 0.8},
	}}
	usage := agents.NewUsageCollector()
	analyst := causeAnalyst{evaluator: evaluator, usage: usage}
	result, err := analyst.AnalyzeCauses(context.Background(), causeRequest(repo, "fixture.go:14", "fixture.go:19", "fixture.go:24", "fixture.go:8"))
	require.NoError(t, err)

	require.Equal(t, 3, evaluator.calls)
	require.Equal(t, []string{"fixture.go:14", "fixture.go:19", "fixture.go:24"}, []string{result.HotPaths[0].Location, result.HotPaths[1].Location, result.HotPaths[2].Location})
	require.Contains(t, result.HotPaths[0].Evidence, "unbuffered_io")
	require.InDelta(t, 0.9, result.HotPaths[0].Confidence, 1e-9)
	// Unbuffered IO outranks the higher raw allocation answer, and every
	// site's first cause comes before any site's second.
	require.Len(t, result.CandidateHypotheses, 3)
	require.Contains(t, result.CandidateHypotheses[0], "bufio")
	require.Contains(t, result.CandidateHypotheses[0], "write (fixture.go:14)")
	require.Contains(t, result.CandidateHypotheses[1], "(*box).get (fixture.go:19)")
	require.Contains(t, result.CandidateHypotheses[2], "heap allocation")
	require.Len(t, result.Targets, 3)
	require.Equal(t, "fixture.go:14", result.Targets[0].Location)
	require.Equal(t, "unbuffered_io", result.Targets[0].Cause)
	require.Equal(t, result.CandidateHypotheses[0], result.Targets[0].Remedy)
	require.Equal(t, "(*box).get", result.Targets[1].Function)
	require.Equal(t, "alloc", result.Targets[2].Cause)
	require.Len(t, result.LikelyCauses, 3)
	require.Contains(t, result.LikelyCauses[0], "many small unbuffered reads or writes")
	require.Len(t, result.AdditionalChecks, 2)
	require.Contains(t, result.AdditionalChecks[0], "(pair).plain): no cause stood out")
	require.Contains(t, result.AdditionalChecks[1], "fixture.go:8: not classified")

	recorded := usage.Snapshot()[string(agents.RoleAnalyst)]
	require.Equal(t, int64(3), recorded.Requests)
	require.Equal(t, int64(2700), recorded.PromptTokens)
}

func TestCauseAnalystKeepsGoingWhenOneSiteFails(t *testing.T) {
	repo := writeCauseFixture(t)
	evaluator := &scriptedEvaluator{fail: map[string]error{"write": errors.New("gateway returned HTTP 429 for Jev")}}
	result, err := causeAnalyst{evaluator: evaluator}.AnalyzeCauses(context.Background(), causeRequest(repo, "fixture.go:14", "fixture.go:19"))
	require.NoError(t, err)
	require.Equal(t, "measured during discovery; not classified", result.HotPaths[0].Evidence)
	require.Contains(t, result.AdditionalChecks[0], "fixture.go:14 (write): not classified: gateway returned HTTP 429")
}

func TestCauseAnalystFailsWhenItClassifiesNothing(t *testing.T) {
	repo := writeCauseFixture(t)
	evaluator := &scriptedEvaluator{partial: true}
	_, err := causeAnalyst{evaluator: evaluator}.AnalyzeCauses(context.Background(), causeRequest(repo, "fixture.go:14"))
	require.ErrorContains(t, err, "classified none of 1 hot functions")
	require.ErrorContains(t, err, "redundant")
}

func TestCauseAnalystWithNothingToClassify(t *testing.T) {
	evaluator := &scriptedEvaluator{}
	result, err := causeAnalyst{evaluator: evaluator}.AnalyzeCauses(context.Background(), causeRequest(t.TempDir(), "runtime.mallocgc"))
	require.NoError(t, err)
	require.Zero(t, evaluator.calls)
	require.Len(t, result.AdditionalChecks, 1)
	require.Contains(t, result.AdditionalChecks[0], "nothing to classify")
}

func TestTokens32Clamps(t *testing.T) {
	require.Equal(t, int32(0), tokens32(-5))
	require.Equal(t, int32(42), tokens32(42))
	require.Equal(t, int32(math.MaxInt32), tokens32(math.MaxInt64))
}

// TestRunADKWithJevAnalyst drives the real graph with the cause analyst in the
// analyst's place: the classification is persisted, its usage reaches the
// report's table, and the report names the analyst.
func TestRunADKWithJevAnalyst(t *testing.T) {
	repo := makeRepository(t)
	engine, err := Create(context.Background(), Options{Repository: repo, ManifestPath: writeManifest(t, t.TempDir()), CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true})
	require.NoError(t, err)
	require.NoError(t, engine.Run(context.Background()))
	defer func() { _ = engine.Close() }()

	roles, err := agents.NewDeterministicSet()
	require.NoError(t, err)
	roles.Usage = agents.NewUsageCollector()
	roles.CauseEvaluator = &scriptedEvaluator{answers: map[string]map[jev.Cause]float64{"main": {jev.CauseUnbufferedIO: 0.8}}}
	engine.SetADK(&roles, nil)
	engine.state.DiscoveryHotFunctions = []string{"main.go:3"}

	_, err = engine.RunADK(context.Background(), roles, orchestrator.Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: time.Minute, AgentTimeout: time.Minute})
	require.NoError(t, err)
	require.Equal(t, AnalystJev, engine.State().Analyst)
	require.Equal(t, int64(1), engine.State().TokenUsage[string(agents.RoleAnalyst)].Requests)
	events, err := engine.store.Events()
	require.NoError(t, err)
	var classified bool
	for _, event := range events {
		if event.Type == "cause_analysis" && strings.Contains(event.Message, "classified 1 of 1") {
			classified = true
		}
	}
	require.True(t, classified, "no cause_analysis event")
	require.Contains(t, RenderMarkdown(engine.State()), "Analyst: Jev cause classification (`typesafe-ai/jev`)")
}
