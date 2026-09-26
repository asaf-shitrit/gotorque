package campaign

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/jev"
	"example.com/gotorque/internal/orchestrator"
	"google.golang.org/genai"
)

// AnalystJev marks a campaign whose analyst was Jev cause classification
// rather than a model role.
const AnalystJev = "jev"

const (
	// maxCauseSites bounds the hot functions classified per cycle. It matches
	// the excerpt budget, so every site the analyst names can also reach the
	// optimizer as source.
	maxCauseSites = maxExcerpts
	// maxCauseSourceBytes skips functions too large to send whole. Jev reads
	// at most 32k tokens of state, and the benchmark behind the baseline never
	// sent a function over 120 lines, so a larger one would be classified
	// outside anything the baseline describes.
	maxCauseSourceBytes = 16 * 1024
)

// objectivePeakMemory is manifest.PerformancePolicy.PrimaryMetric under
// --tradeoff lean (see internal/manifest/tradeoff.go). A campaign judged by it
// ranks allocation causes ahead of others within each targets() tier, since
// discovery's hot list is CPU-derived by default and a CPU-hot function need
// not allocate at all (ADR 0024).
const objectivePeakMemory = "peak_memory_bytes"

// causeAnalyst is the deterministic analyst selected by --analyst jev. It asks
// Jev the same yes/no questions about every measured hot function, ranks each
// function's answers against Jev's usual answers, and turns the causes that
// stand out into the analysis the optimizer reads. Jev supplies the evidence;
// this code does the ranking, and nothing here reaches the policy decision.
type causeAnalyst struct {
	engine    *Engine
	evaluator jev.Evaluator
	usage     *agents.UsageCollector
}

// hotFunction is one measured hot function with its whole source.
type hotFunction struct {
	Location string `json:"location"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Source   string `json:"-"`
}

// siteVerdict is the classification of one hot function, persisted with the
// cause_analysis event so a report reader can see why a hypothesis was made.
type siteVerdict struct {
	Site    hotFunction `json:"site"`
	Scores  []jev.Score `json:"scores,omitempty"`
	Flagged []jev.Score `json:"flagged,omitempty"`
	// Overruled names flags code removed after Jev raised them, with why.
	Overruled []string `json:"overruled,omitempty"`
	// Kinds is the fix kind Jev chose within each flagged cause that has
	// kinds and whose leader cleared jev.KindGate; KindScores are the answers.
	Kinds      map[jev.Cause]jev.FixKind `json:"kinds,omitempty"`
	KindScores []jev.KindScore           `json:"kind_scores,omitempty"`
	Problem    string                    `json:"problem,omitempty"`
}

func (a causeAnalyst) AnalyzeCauses(ctx context.Context, req orchestrator.CauseRequest) (agents.AnalystResult, error) {
	sites, skipped := hotFunctions(req.Campaign.Repository, req.Discovery.HotFunctions)
	if len(sites) == 0 {
		return agents.AnalystResult{AdditionalChecks: append(skipped, "discovery measured no hot function with a readable source position, so there was nothing to classify")}, nil
	}
	verdicts := make([]siteVerdict, 0, len(sites))
	classified := 0
	for _, site := range sites {
		verdict := a.chooseKinds(ctx, flagWithVetoes(req.Campaign.Repository, a.classify(ctx, site)))
		if verdict.Problem == "" {
			classified++
		}
		verdicts = append(verdicts, verdict)
	}
	a.record(classified, verdicts)
	if classified == 0 {
		return agents.AnalystResult{}, fmt.Errorf("jev classified none of %d hot functions; first failure: %s", len(sites), verdicts[0].Problem)
	}
	result := analystResult(verdicts, req.Campaign.Objective)
	result.AdditionalChecks = append(result.AdditionalChecks, skipped...)
	addThrowawayTargets(req.Campaign.Repository, sites, req.Discovery.HotFunctionWeights, &result)
	return result, nil
}

// addThrowawayTargets runs the code-only throwaway_result signal (ADR 0027)
// over the same hot functions Jev classified and puts every target it fires
// on first, ahead of every Jev-ranked target: the evidence is structural and
// deterministic, not a probability estimate, so it does not wait its turn in
// Jev's tiers (ADR 0018). It is never asked of Jev and never touches a
// siteVerdict, so it cannot reach Jev's state or baseline. weights is
// discovery's per-function profile hotness, used only to rank a target's
// consuming callers before the cap (see consumingCallers); a nil or empty map
// leaves the ranking exactly as it was before this field existed.
func addThrowawayTargets(repo string, sites []hotFunction, weights map[string]float64, result *agents.AnalystResult) {
	tw := throwawayTargets(repo, sites, weights)
	if len(tw) == 0 {
		return
	}
	result.Targets = append(tw, result.Targets...)
	remedies := make([]string, 0, len(tw))
	for _, t := range tw {
		remedies = append(remedies, t.Remedy)
	}
	result.CandidateHypotheses = append(remedies, result.CandidateHypotheses...)
	result.HotPaths = append(result.HotPaths, throwawayExcerptPaths(tw)...)
}

func (a causeAnalyst) classify(ctx context.Context, site hotFunction) siteVerdict {
	resp, err := a.evaluator.Evaluate(ctx, jev.Request{State: jev.SiteState(site.Path, site.Source), Questions: jev.Questions()})
	if err != nil {
		return siteVerdict{Site: site, Problem: err.Error()}
	}
	// Jev bills input tokens only; recording them under the analyst role puts
	// its requests in the report's usage table beside the model roles.
	a.recordUsage(resp.Usage)
	scores, err := jev.Rank(resp.Answers)
	if err != nil {
		return siteVerdict{Site: site, Problem: err.Error()}
	}
	return siteVerdict{Site: site, Scores: scores, Flagged: jev.Flagged(scores)}
}

// chooseKinds asks the fix-kind questions for a site with a flagged cause that
// has kinds, in one request over the same state the causes were asked about,
// and keeps each cause's kind that clears jev.KindGate. A failed request, or no
// kind standing out, leaves the cause's generic remedy: the answer narrows the
// optimizer's instruction and nothing else.
func (a causeAnalyst) chooseKinds(ctx context.Context, v siteVerdict) siteVerdict {
	if v.Problem != "" || !slices.ContainsFunc(v.Flagged, func(s jev.Score) bool { return jev.HasKinds(s.Cause) }) {
		return v
	}
	resp, err := a.evaluator.Evaluate(ctx, jev.Request{State: jev.SiteState(v.Site.Path, v.Site.Source), Questions: jev.KindQuestions()})
	if err != nil {
		return v
	}
	a.recordUsage(resp.Usage)
	for _, s := range v.Flagged {
		if !jev.HasKinds(s.Cause) {
			continue
		}
		kind, scores, ok, err := jev.ChooseKind(s.Cause, resp.Answers)
		if err != nil {
			continue
		}
		v.KindScores = append(v.KindScores, scores...)
		if ok {
			if v.Kinds == nil {
				v.Kinds = map[jev.Cause]jev.FixKind{}
			}
			v.Kinds[s.Cause] = kind
		}
	}
	return v
}

func (a causeAnalyst) recordUsage(u jev.Usage) {
	a.usage.Record(string(agents.RoleAnalyst), &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     tokens32(u.InputTokens),
		CandidatesTokenCount: tokens32(u.OutputTokens),
		TotalTokenCount:      tokens32(u.InputTokens + u.OutputTokens),
	})
}

func tokens32(n int64) int32 {
	if n <= 0 {
		return 0
	}
	if n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

func (a causeAnalyst) record(classified int, verdicts []siteVerdict) {
	if a.engine == nil {
		return
	}
	_ = a.engine.saveEvent("cause_analysis", fmt.Sprintf("Jev classified %d of %d hot functions", classified, len(verdicts)), verdicts)
}

// hotFunctions resolves discovery's locations to whole functions, hottest
// first, one entry per function. Bare symbol names carry no position and are
// skipped as the excerpt collector skips them; a position that does not
// resolve is reported so the analysis says what it could not look at.
func hotFunctions(repo string, locations []string) ([]hotFunction, []string) {
	var sites []hotFunction
	var skipped []string
	seen := map[string]bool{}
	for _, location := range locations {
		if len(sites) == maxCauseSites {
			break
		}
		path, line, ok := parseLocation(location)
		if !ok || line == 0 || !strings.HasSuffix(path, ".go") {
			continue
		}
		fn, start, err := functionAt(filepath.Join(repo, path), line)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: not classified: %v", location, err))
			continue
		}
		key := path + ":" + strconv.Itoa(start)
		if seen[key] {
			continue
		}
		seen[key] = true
		fn.Location, fn.Path = location, filepath.ToSlash(path)
		sites = append(sites, fn)
	}
	return sites, skipped
}

// functionAt returns the top-level function declaration containing line, with
// its doc comment, which is the unit the baseline was measured on.
func functionAt(file string, line int) (hotFunction, int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return hotFunction{}, 0, err
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, data, parser.ParseComments)
	if err != nil {
		return hotFunction{}, 0, err
	}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || line < fset.Position(fd.Pos()).Line || line > fset.Position(fd.End()).Line {
			continue
		}
		from := fd.Pos()
		if fd.Doc != nil {
			from = fd.Doc.Pos()
		}
		source := data[fset.Position(from).Offset:fset.Position(fd.End()).Offset]
		if len(source) > maxCauseSourceBytes {
			return hotFunction{}, 0, fmt.Errorf("function %s is %d bytes, over the %d-byte classification limit", funcName(fd), len(source), maxCauseSourceBytes)
		}
		return hotFunction{Name: funcName(fd), Source: string(source)}, fset.Position(fd.Pos()).Line, nil
	}
	return hotFunction{}, 0, errors.New("no function declaration contains line " + strconv.Itoa(line))
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return fd.Name.Name
	}
	return "(" + receiverType(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverType(t.X)
	case *ast.IndexListExpr:
		return receiverType(t.X)
	}
	return "?"
}

// analystResult turns the verdicts into the analysis the optimizer reads. Hot
// paths keep discovery's locations verbatim, which the excerpt collector
// needs and a model analyst used to reformat.
func analystResult(verdicts []siteVerdict, objective string) agents.AnalystResult {
	var result agents.AnalystResult
	for _, v := range verdicts {
		result.HotPaths = append(result.HotPaths, hotPath(v))
		site := fmt.Sprintf("%s (%s)", v.Site.Location, v.Site.Name)
		switch {
		case v.Problem != "":
			result.AdditionalChecks = append(result.AdditionalChecks, site+": not classified: "+v.Problem)
		case len(v.Flagged) == 0:
			result.AdditionalChecks = append(result.AdditionalChecks, site+": no cause stood out from Jev's usual answers; read it before patching it")
		}
		for _, s := range v.Flagged {
			result.LikelyCauses = append(result.LikelyCauses, fmt.Sprintf("%s: %s (Jev yes %.2f, %+.1f sd from its usual answer)", site, s.Cause.Summary(), s.Probability, s.Z))
		}
	}
	result.Targets = targets(verdicts, objective)
	for _, t := range result.Targets {
		result.CandidateHypotheses = append(result.CandidateHypotheses, t.Remedy)
	}
	return result
}

// targets lists every flagged cause as a target, every site's first cause
// before any site's second cause. Within a tier, a cause is ordered by its z
// discounted by how far its function sits from the top of the profile,
// z/sqrt(1+rank): strong evidence a few places down outranks weak evidence at
// the top, and far down the list only much stronger evidence does. The
// orchestrator attacks them in this order, one per candidate.
//
// Hotness alone sent a live gojq campaign, once a realistic workload was
// measured, to three fast-path flags at +1.4 to +1.7 sd on its hottest
// functions, all inconclusive, before printValues' unbuffered output at +3.4
// sd, a bufio fix measured by hand at -12.8%. z alone would have sent gron to
// a +3.97 sd fast path in statementsFromJSON before the +3.47 sd bufio fix of
// its output loop, the patch accepted at -19%. The discounted order puts the
// known fix first in every recorded analysis, on both targets.
//
// When objective is peak memory (ADR 0024), each tier is additionally split
// into allocation causes (alloc, prealloc, string_build; see
// jev.Cause.IsAllocation) ahead of every other cause, the z/sqrt(1+rank) order
// kept inside each half. Discovery's hot list is still built from CPU
// evidence, so a lean campaign otherwise finds memory wins only where the
// CPU-hot code it was handed also happens to allocate; this ranking puts a
// site's allocation-flagged cause first whenever one was flagged at all.
func targets(verdicts []siteVerdict, objective string) []agents.Target {
	out := make([]agents.Target, 0, len(verdicts)*jev.MaxFlagged)
	memory := objective == objectivePeakMemory
	for rank := range jev.MaxFlagged + 1 {
		tier := rankTier(verdicts, rank)
		slices.SortStableFunc(tier, func(a, b rankedTarget) int { return compareRanked(a, b, memory) })
		for _, r := range tier {
			out = append(out, r.target)
		}
	}
	return out
}

// rankedTarget is one tier's candidate target, its z/sqrt(1+rank) weight, and
// whether its cause is allocation-related, which compareRanked uses to sort
// the tier under the peak-memory objective.
type rankedTarget struct {
	target agents.Target
	weight float64
	alloc  bool
}

// rankTier collects every verdict's rank'th flagged cause into one tier.
//
// The tier after the last rank holds every deferred cause (jev.Deferred), so
// fast_path is attacked only once every other target has been tried (ADR 0025).
func rankTier(verdicts []siteVerdict, rank int) []rankedTarget {
	var tier []rankedTarget
	for i, v := range verdicts {
		for _, s := range tierFlags(v.Flagged, rank) {
			tier = append(tier, rankedTarget{
				target: siteTarget(v, s),
				weight: s.Z / math.Sqrt(float64(1+i)),
				alloc:  s.Cause.IsAllocation(),
			})
		}
	}
	return tier
}

// tierFlags is a verdict's flag at the given rank among its non-deferred flags,
// or, at rank MaxFlagged, its deferred ones.
func tierFlags(flagged []jev.Score, rank int) []jev.Score {
	var primary, deferred []jev.Score
	for _, s := range flagged {
		if jev.Deferred(s.Cause) {
			deferred = append(deferred, s)
		} else {
			primary = append(primary, s)
		}
	}
	switch {
	case rank == jev.MaxFlagged:
		return deferred
	case rank < len(primary):
		return primary[rank : rank+1]
	}
	return nil
}

// compareRanked orders a tier by weight, discounted z/sqrt(1+rank), unless
// memory is set and the two targets disagree on IsAllocation, in which case
// the allocation cause leads regardless of weight (ADR 0024).
func compareRanked(a, b rankedTarget, memory bool) int {
	if memory && a.alloc != b.alloc {
		if a.alloc {
			return -1
		}
		return 1
	}
	return cmp.Compare(b.weight, a.weight)
}

// siteTarget is one flagged cause as a target, with the remedy of the fix kind
// Jev chose within it when one stood out.
func siteTarget(v siteVerdict, s jev.Score) agents.Target {
	t := agents.Target{Location: v.Site.Location, Function: v.Site.Name, Cause: string(s.Cause), Remedy: s.Cause.Remedy(v.Site.Name, v.Site.Location), Z: s.Z}
	if kind, ok := v.Kinds[s.Cause]; ok {
		t.FixKind, t.Remedy = string(kind), kind.Remedy(v.Site.Name, v.Site.Location)
	}
	return t
}

func hotPath(v siteVerdict) agents.HotPath {
	hp := agents.HotPath{Location: v.Site.Location, Evidence: "measured during discovery; not classified"}
	if v.Problem != "" {
		return hp
	}
	parts := make([]string, 0, 3)
	for _, s := range v.Scores[:min(3, len(v.Scores))] {
		parts = append(parts, fmt.Sprintf("%s %+.1f sd (yes %.2f)", s.Cause, s.Z, s.Probability))
	}
	hp.Evidence = "measured during discovery; Jev relative to its usual answers: " + strings.Join(parts, ", ")
	if len(v.Flagged) > 0 {
		hp.Confidence = v.Flagged[0].Probability
	}
	return hp
}
