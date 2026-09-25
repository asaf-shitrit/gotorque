package campaign

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
)

// throwaway_result (ADR 0027) is a code-derived cause, not a Jev cause: code
// alone decides whether it fires, from the same source Jev is given, and it
// never reaches Jev's question set or baseline. It targets a hot function
// that allocates a fresh result its own package mostly throws away: a callee
// that returns a fresh *T, called by several sites in the same directory that
// only read one field or call one cheap method on the result before dropping
// it. The dasel campaign is the motivating case (docs/adr/0027): profiling
// ranked (*Value).UnpackKinds as the hottest function, ending every call with
// a fresh *Value about ten of its package's own callers used only to read a
// Kind or call one predicate.
const causeThrowawayResult = "throwaway_result"

// callSiteUsage classifies how one call site uses a callee's result.
type callSiteUsage string

const (
	usageDiscarded callSiteUsage = "discarded"
	usageConsumed  callSiteUsage = "consumed"
	usageEscapes   callSiteUsage = "escapes"
)

// callSite is one call to the callee, found in the same package directory.
type callSite struct {
	Caller   string        `json:"caller"`
	Location string        `json:"location"`
	Usage    callSiteUsage `json:"usage"`
}

// throwawayAnalysis is one hot function's caller analysis: whether it returns
// a fresh allocation on every path, and every call site to it found in its
// own package directory.
type throwawayAnalysis struct {
	Callee string
	Fresh  bool
	Sites  []callSite
}

// signal reports whether throwaway_result fires: the callee returns a fresh
// allocation on every return path, at least two call sites are consumed or
// discarded, and they are at least half of every call site found. The count
// gate keeps a single incidental call (an isolated site, or one of many that
// use the result normally) from firing the signal; the fraction gate keeps a
// callee with one throwaway caller among a hundred real ones from firing it.
func (a throwawayAnalysis) signal() bool {
	if !a.Fresh || len(a.Sites) == 0 {
		return false
	}
	n := a.consumingCount()
	return n >= 2 && n*2 >= len(a.Sites)
}

func (a throwawayAnalysis) consumingCount() int {
	n := 0
	for _, s := range a.Sites {
		if s.Usage == usageConsumed || s.Usage == usageDiscarded {
			n++
		}
	}
	return n
}

// consumingCallers is every consumed or discarded call site, ranked by call
// count (a caller with more than one call site to the callee ranks first)
// and then by source position, for a stable and reproducible target set.
func (a throwawayAnalysis) consumingCallers() []callSite {
	counts := map[string]int{}
	for _, s := range a.Sites {
		if s.Usage == usageConsumed || s.Usage == usageDiscarded {
			counts[s.Caller]++
		}
	}
	seen := map[string]bool{}
	var out []callSite
	for _, s := range a.Sites {
		if s.Usage != usageConsumed && s.Usage != usageDiscarded {
			continue
		}
		if seen[s.Caller] {
			continue
		}
		seen[s.Caller] = true
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(x, y callSite) int {
		return cmp.Compare(counts[y.Caller], counts[x.Caller])
	})
	return out
}

// analyzeThrowaway finds every call to site within its package directory
// (same directory as site.Path, every non-test .go file) and classifies each
// one. Methods are matched by method name on a selector call only; the
// prototype accepts the ambiguity of two same-named methods on different
// receiver types in one package colliding, and says so here rather than
// resolving types.
func analyzeThrowaway(repo string, site hotFunction) (throwawayAnalysis, error) {
	pkgDir := path.Dir(site.Path)
	dir := filepath.Join(repo, filepath.FromSlash(pkgDir))
	pkgFiles, pkgName, err := parsePackageDir(dir, pkgDir)
	if err != nil {
		return throwawayAnalysis{}, err
	}
	calleeName := methodOrFuncName(site.Name)
	callee, calleeFile := findDeclAndFile(pkgFiles, site.Name)
	if callee == nil || callee.Body == nil {
		return throwawayAnalysis{}, nil
	}
	freeFuncs := freeFunctionsByName(pkgFiles)
	fresh := returnsFreshAllocation(callee, freeFuncs)
	var sites []callSite
	for _, pf := range pkgFiles {
		if pf.file.Name.Name != pkgName {
			continue
		}
		sites = append(sites, callSitesIn(pf, callee, calleeFile, calleeName)...)
	}
	slices.SortFunc(sites, func(a, b callSite) int {
		if c := strings.Compare(locationFile(a.Location), locationFile(b.Location)); c != 0 {
			return c
		}
		return cmp.Compare(locationLine(a.Location), locationLine(b.Location))
	})
	return throwawayAnalysis{Callee: site.Name, Fresh: fresh, Sites: sites}, nil
}

// methodOrFuncName is the bare identifier a call site spells: the part after
// the receiver in "(*Value).UnpackKinds", or the name itself for a plain
// function.
func methodOrFuncName(funcNameFormatted string) string {
	if i := strings.LastIndex(funcNameFormatted, "."); i >= 0 {
		return funcNameFormatted[i+1:]
	}
	return funcNameFormatted
}

type pkgFile struct {
	path string // OS path, for identity comparisons
	rel  string // repository-relative, slash-separated: "model/value.go"
	fset *token.FileSet
	file *ast.File
}

// parsePackageDir parses every non-test .go file in dir with object
// resolution enabled, which is what lets consumedLocal below match a local's
// later uses back to the identifier that declared it. pkgDir is dir's
// repository-relative, slash-separated form ("." at the repository root),
// used to build each file's rel path in the same format hotFunctions and
// parseLocation already use.
func parsePackageDir(dir, pkgDir string) ([]pkgFile, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	var out []pkgFile
	pkgName := ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, full, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		if pkgName == "" {
			pkgName = file.Name.Name
		}
		if file.Name.Name != pkgName {
			continue
		}
		out = append(out, pkgFile{path: full, rel: path.Join(pkgDir, e.Name()), fset: fset, file: file})
	}
	return out, pkgName, nil
}

func findDeclAndFile(pkgFiles []pkgFile, name string) (*ast.FuncDecl, *pkgFile) {
	for i := range pkgFiles {
		if fd := findFunc(pkgFiles[i].file, name); fd != nil {
			return fd, &pkgFiles[i]
		}
	}
	return nil, nil
}

// freeFunctionsByName indexes every receiver-less top-level function by name,
// for returnsFreshAllocation's one-level check of what a callee's own return
// calls.
func freeFunctionsByName(pkgFiles []pkgFile) map[string]*ast.FuncDecl {
	out := map[string]*ast.FuncDecl{}
	for _, pf := range pkgFiles {
		for _, decl := range pf.file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				out[fd.Name.Name] = fd
			}
		}
	}
	return out
}

// returnsFreshAllocation reports whether every return statement in fd
// (ignoring nested function literals, which return to their own caller, not
// fd's) returns a freshly allocated value: the address of a composite
// literal, new(T), or a call to a same-package free function whose own
// returns are checked one level deep (returnsQualify's strict=false): that
// second level accepts a composite literal, new(T), or a call to a same-
// package free function too, but does not open that third function's body.
// One level deep is what dasel's UnpackKinds needs: its own `return
// NewValue(res)` opens NewValue, whose four branches are two composite
// literals and two calls (NewNestedValue, NewNullValue) that are trusted at
// that second level rather than opened themselves -- NewNullValue calls
// NewValue back, and a third level would recurse forever.
func returnsFreshAllocation(fd *ast.FuncDecl, freeFuncs map[string]*ast.FuncDecl) bool {
	return returnsQualify(fd, freeFuncs, true)
}

// returnsQualify is returnsFreshAllocation's implementation. strict controls
// what a call to a same-package function needs to qualify: true (the top
// level) opens that function's body and requires every one of its own
// returns to qualify, checked with strict=false; false (one level down)
// trusts any call to a known same-package function outright, so the
// recursion never goes past one level.
func returnsQualify(fd *ast.FuncDecl, freeFuncs map[string]*ast.FuncDecl, strict bool) bool {
	if fd.Body == nil {
		return false
	}
	found, ok := false, true
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if !ok {
			return false
		}
		if _, isLit := n.(*ast.FuncLit); isLit {
			return false
		}
		ret, isRet := n.(*ast.ReturnStmt)
		if !isRet {
			return true
		}
		found = true
		if len(ret.Results) != 1 || !isFreshAllocExpr(ret.Results[0], freeFuncs, strict) {
			ok = false
		}
		return true
	})
	return found && ok
}

func isFreshAllocExpr(expr ast.Expr, freeFuncs map[string]*ast.FuncDecl, strict bool) bool {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			_, isComposite := e.X.(*ast.CompositeLit)
			return isComposite
		}
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		if !ok {
			return false
		}
		if id.Name == "new" && len(e.Args) == 1 {
			return true
		}
		callee, known := freeFuncs[id.Name]
		if !known {
			return false
		}
		if !strict {
			return true
		}
		return returnsQualify(callee, freeFuncs, false)
	}
	return false
}

// callSitesIn finds every call to calleeName in pf whose caller function is
// not the callee itself (calleeFile/callee together identify the callee's
// own declaration, so a recursive call inside it is never reported as a
// caller).
func callSitesIn(pf pkgFile, callee *ast.FuncDecl, calleeFile *pkgFile, calleeName string) []callSite {
	var sites []callSite
	for _, decl := range pf.file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil || (fd == callee && calleeFile != nil && pf.path == calleeFile.path) {
			continue
		}
		caller := funcName(fd)
		parents := parentsOf(fd.Body)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !callsName(call, calleeName) {
				return true
			}
			sites = append(sites, callSite{
				Caller:   caller,
				Location: pf.rel + ":" + strconv.Itoa(pf.fset.Position(call.Pos()).Line),
				Usage:    classifyUsage(fd.Body, call, parents),
			})
			return true
		})
	}
	return sites
}

// callsName reports whether call invokes a function or method named name: a
// bare identifier, or the selector of a method call.
func callsName(call *ast.CallExpr, name string) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == name
	case *ast.SelectorExpr:
		return fn.Sel.Name == name
	}
	return false
}

// parentsOf maps every node under root to its immediate parent, which
// classifyUsage and consumedLocal use to tell how a call's result is used
// without a full type-checked AST.
func parentsOf(root ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parents
}

// classifyUsage implements the classification in internal/campaign/callers.go's
// package doc: consumed when the call is the X of a selector in the same
// expression, or is assigned to a local whose every later use is such a
// selector's X; discarded when the call's result is dropped outright;
// escapes otherwise.
func classifyUsage(body *ast.BlockStmt, call *ast.CallExpr, parents map[ast.Node]ast.Node) callSiteUsage {
	parent := parents[call]
	if sel, ok := parent.(*ast.SelectorExpr); ok && sel.X == ast.Expr(call) {
		return usageConsumed
	}
	if _, ok := parent.(*ast.ExprStmt); ok {
		return usageDiscarded
	}
	if assign, ok := parent.(*ast.AssignStmt); ok {
		return classifyAssign(body, assign, parents)
	}
	return usageEscapes
}

// classifyAssign handles `x := callee(...)` and `x = callee(...)`: discarded
// for `_ = callee(...)`, consumed when x is a plain identifier whose every
// other use in body is as a selector's X, escapes for anything else
// (multi-value assignment, a field or index on the left, or a use that is not
// a bare selector access).
func classifyAssign(body *ast.BlockStmt, assign *ast.AssignStmt, parents map[ast.Node]ast.Node) callSiteUsage {
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return usageEscapes
	}
	lhs, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return usageEscapes
	}
	if lhs.Name == "_" {
		return usageDiscarded
	}
	if consumedLocal(body, lhs, parents) {
		return usageConsumed
	}
	return usageEscapes
}

// consumedLocal reports whether every other identifier in body resolving to
// the same object as decl (go/parser's local object resolution, which is why
// parsePackageDir parses without SkipObjectResolution) is used only as the X
// of a selector expression: a field read or a method call, never stored,
// returned, passed as an argument, or addressed.
func consumedLocal(body *ast.BlockStmt, decl *ast.Ident, parents map[ast.Node]ast.Node) bool {
	if decl.Obj == nil {
		return false
	}
	consumed := true
	ast.Inspect(body, func(n ast.Node) bool {
		if !consumed {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || id == decl || id.Obj != decl.Obj {
			return true
		}
		sel, ok := parents[id].(*ast.SelectorExpr)
		if !ok || sel.X != ast.Expr(id) {
			consumed = false
		}
		return true
	})
	return consumed
}

func locationFile(loc string) string {
	file, _, _ := strings.Cut(loc, ":")
	return file
}

func locationLine(loc string) int {
	_, lineStr, ok := strings.Cut(loc, ":")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(lineStr)
	return n
}

// throwawayTarget builds the multi-function target for a throwaway_result
// signal: the callee plus its ranked consuming callers, capped at
// maxThrowawayFunctions functions total (ADR 0027 raises this above the
// six originally proposed; see the ADR for why).
func throwawayTarget(site hotFunction, a throwawayAnalysis) agents.Target {
	callers := a.consumingCallers()
	if limit := maxThrowawayFunctions - 1; len(callers) > limit {
		callers = callers[:limit]
	}
	fns := make([]agents.FunctionRef, 0, len(callers))
	names := make([]string, 0, len(callers))
	for _, c := range callers {
		fns = append(fns, agents.FunctionRef{Name: c.Caller, Location: c.Location})
		names = append(names, c.Caller)
	}
	remedy := throwawayRemedy(site.Name, names)
	return agents.Target{
		Location:  site.Location,
		Function:  site.Name,
		Cause:     causeThrowawayResult,
		Remedy:    remedy,
		Functions: agents.EncodeFunctionSet(fns),
	}
}

func throwawayRemedy(callee string, callers []string) string {
	return "Profiling and the call sites in " + callee + "'s own package agree: " + callee +
		" allocates a fresh result on every call, and " + strings.Join(callers, ", ") +
		" only read a field or call one cheap method on it before dropping it. Add a" +
		" non-allocating variant beside " + callee + " that returns what those callers" +
		" need (the unwrapped value, not a new wrapper around it), switch " + strings.Join(callers, ", ") +
		" to call it instead, and keep " + callee + "'s existing behaviour for every other caller."
}

const maxThrowawayFunctions = 12

// throwawayTargets runs the throwaway_result signal over every profiled hot
// function and returns one target per function it fires on, ranked by how
// many consuming callers were found (the strongest structural evidence
// first). It is code-derived and independent of Jev: it reads the same
// sources hotFunctions already resolved, and never asks Jev anything.
func throwawayTargets(repo string, sites []hotFunction) []agents.Target {
	type found struct {
		target agents.Target
		count  int
	}
	var hits []found
	for _, site := range sites {
		a, err := analyzeThrowaway(repo, site)
		if err != nil || !a.signal() {
			continue
		}
		hits = append(hits, found{target: throwawayTarget(site, a), count: a.consumingCount()})
	}
	slices.SortStableFunc(hits, func(a, b found) int { return cmp.Compare(b.count, a.count) })
	out := make([]agents.Target, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.target)
	}
	return out
}

// throwawayExcerptPaths is every location a throwaway_result target's brief
// needs source for: the callee and every function in its set, so
// extractExcerpts (which is driven entirely by agents.HotPath.Location) picks
// up the callers' source too, not just the callee's.
func throwawayExcerptPaths(targets []agents.Target) []agents.HotPath {
	var out []agents.HotPath
	for _, t := range targets {
		if t.Cause != causeThrowawayResult {
			continue
		}
		out = append(out, agents.HotPath{Location: t.Location, Evidence: "code-derived: throwaway_result"})
		for _, f := range agents.DecodeFunctionSet(t.Functions) {
			out = append(out, agents.HotPath{Location: f.Location, Evidence: "code-derived: throwaway_result caller"})
		}
	}
	return out
}
