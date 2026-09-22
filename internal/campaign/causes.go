package campaign

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
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
	Problem string      `json:"problem,omitempty"`
}

func (a causeAnalyst) AnalyzeCauses(ctx context.Context, req orchestrator.CauseRequest) (agents.AnalystResult, error) {
	sites, skipped := hotFunctions(req.Campaign.Repository, req.Discovery.HotFunctions)
	if len(sites) == 0 {
		return agents.AnalystResult{AdditionalChecks: append(skipped, "discovery measured no hot function with a readable source position, so there was nothing to classify")}, nil
	}
	verdicts := make([]siteVerdict, 0, len(sites))
	classified := 0
	for _, site := range sites {
		verdict := a.classify(ctx, site)
		if verdict.Problem == "" {
			classified++
		}
		verdicts = append(verdicts, verdict)
	}
	a.record(classified, verdicts)
	if classified == 0 {
		return agents.AnalystResult{}, fmt.Errorf("jev classified none of %d hot functions; first failure: %s", len(sites), verdicts[0].Problem)
	}
	result := analystResult(verdicts)
	result.AdditionalChecks = append(result.AdditionalChecks, skipped...)
	return result, nil
}

func (a causeAnalyst) classify(ctx context.Context, site hotFunction) siteVerdict {
	resp, err := a.evaluator.Evaluate(ctx, jev.Request{State: jev.SiteState(site.Path, site.Source), Questions: jev.Questions()})
	if err != nil {
		return siteVerdict{Site: site, Problem: err.Error()}
	}
	// Jev bills input tokens only; recording them under the analyst role puts
	// its requests in the report's usage table beside the model roles.
	a.usage.Record(string(agents.RoleAnalyst), &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     tokens32(resp.Usage.InputTokens),
		CandidatesTokenCount: tokens32(resp.Usage.OutputTokens),
		TotalTokenCount:      tokens32(resp.Usage.InputTokens + resp.Usage.OutputTokens),
	})
	scores, err := jev.Rank(resp.Answers)
	if err != nil {
		return siteVerdict{Site: site, Problem: err.Error()}
	}
	return siteVerdict{Site: site, Scores: scores, Flagged: jev.Flagged(scores)}
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
func analystResult(verdicts []siteVerdict) agents.AnalystResult {
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
	result.CandidateHypotheses = hypotheses(verdicts)
	return result
}

// hypotheses lists every flagged cause as a remedy, first causes of every site
// in hotness order before any site's second cause: the hottest function's
// best-supported cause is the likeliest to move the measured wall time.
func hypotheses(verdicts []siteVerdict) []string {
	var out []string
	for rank := range jev.MaxFlagged {
		for _, v := range verdicts {
			if rank < len(v.Flagged) {
				out = append(out, v.Flagged[rank].Cause.Remedy(v.Site.Name, v.Site.Location))
			}
		}
	}
	return out
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
