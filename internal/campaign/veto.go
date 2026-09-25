package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"

	"example.com/gotorque/internal/jev"
)

// codeVetoes are the causes whose mechanism code can see in a function's
// source, and so can rule out (ADR 0025). Jev reads the same source, and on the
// benchmark code-derived features mostly duplicated what it already knew; for
// these two they do not. The mechanism is either present or it is not: every
// benchmark unbuffered-I/O fix changed a real read or write call, and 82% of
// preallocation fixes grew a container inside a loop.
var codeVetoes = map[jev.Cause]struct {
	present func(fd *ast.FuncDecl, types map[string]string) bool
	reason  string
}{
	jev.CauseUnbufferedIO: {hasRealIO, "unbuffered_io: vetoed, no read or write in the function reaches a real file or stream"},
	jev.CausePrealloc:     {growsInLoop, "prealloc: vetoed, nothing in the function grows inside a loop"},
}

// flagWithVetoes flags a verdict's causes through jev.Flag, excluding a cause
// whose mechanism code cannot find in the function, and names every cause it
// excluded, or skipped for Jev's own probability, in Overruled. A function the
// parser cannot find is never vetoed: doubt leaves Jev's answer standing.
func flagWithVetoes(repo string, v siteVerdict) siteVerdict {
	if v.Problem != "" {
		return v
	}
	fd, types := parsedSite(repo, v.Site)
	vetoed := func(c jev.Cause) bool {
		veto, ok := codeVetoes[c]
		return ok && fd != nil && !veto.present(fd, types)
	}
	v.Flagged = jev.Flag(v.Scores, vetoed)
	v.Overruled = overruledReasons(v.Scores, v.Flagged, vetoed)
	return v
}

func overruledReasons(scores, flagged []jev.Score, vetoed func(jev.Cause) bool) []string {
	var reasons []string
	for _, s := range scores {
		if s.Z < jev.FlagThreshold {
			break
		}
		if slices.ContainsFunc(flagged, func(f jev.Score) bool { return f.Cause == s.Cause }) {
			continue
		}
		switch {
		case vetoed(s.Cause):
			reasons = append(reasons, codeVetoes[s.Cause].reason)
		case s.Probability < jev.ProbabilityFloor:
			reasons = append(reasons, string(s.Cause)+": skipped, Jev's own answer was below "+strconv.FormatFloat(jev.ProbabilityFloor, 'g', -1, 64))
		}
	}
	return reasons
}

// parsedSite finds the site's declaration and the types its receiver fields,
// parameters and locals are declared with, or nil when it cannot.
func parsedSite(repo string, site hotFunction) (*ast.FuncDecl, map[string]string) {
	full := filepath.Join(repo, filepath.FromSlash(site.Path))
	file, err := parser.ParseFile(token.NewFileSet(), full, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil
	}
	fd := findFunc(file, site.Name)
	if fd == nil || fd.Body == nil {
		return nil, nil
	}
	types := localTypes(fd)
	if recv, typeName := receiverOf(fd); recv != "" {
		for field, t := range structFields(filepath.Dir(full), file.Name.Name, typeName) {
			types[recv+"."+field] = t
		}
	}
	return fd, types
}

// printers write to a stream without naming it: fmt.Print* and log.Print*.
var printers = map[string]bool{
	"fmt.Print": true, "fmt.Printf": true, "fmt.Println": true,
	"log.Print": true, "log.Printf": true, "log.Println": true,
}

// hasRealIO reports a read or write call whose destination is not provably an
// in-memory buffer, or a print to the process's own streams. It generalises
// onlyInMemoryIO: a function with no I/O call at all has nothing for a
// buffering remedy to batch either.
func hasRealIO(fd *ast.FuncDecl, types map[string]string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && printers[typeName(sel)] {
			found = true
			return false
		}
		if dst, isIO := ioDestination(call); isIO && !inMemoryTypes[types[exprKey(dst)]] {
			found = true
		}
		return !found
	})
	return found
}

// growsInLoop reports a slice appended to, or a map written into, inside a
// loop, when code cannot see that the container was made with a size: the
// shape every preallocation remedy changes.
func growsInLoop(fd *ast.FuncDecl, _ map[string]string) bool {
	sized := sizedLocals(fd)
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch loop := n.(type) {
		case *ast.ForStmt:
			found = growsIn(loop.Body, sized)
		case *ast.RangeStmt:
			found = growsIn(loop.Body, sized)
		}
		return !found
	})
	return found
}

func growsIn(body *ast.BlockStmt, sized map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || found {
			return !found
		}
		for i, lhs := range assign.Lhs {
			if growsTarget(lhs, assign, i, sized) {
				found = true
			}
		}
		return !found
	})
	return found
}

// growsTarget reports `x = append(x, ...)` or `m[k] = v` on a container not
// known to be sized.
func growsTarget(lhs ast.Expr, assign *ast.AssignStmt, i int, sized map[string]bool) bool {
	if index, ok := lhs.(*ast.IndexExpr); ok {
		return !sized[exprKey(index.X)] && assign.Tok == token.ASSIGN
	}
	if i >= len(assign.Rhs) {
		return false
	}
	call, ok := assign.Rhs[i].(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	return ok && fn.Name == "append" && !sized[exprKey(lhs)]
}

// sizedLocals names the locals made with an explicit size: make with a length
// or capacity argument. A map written by index is only sized that way too; a
// map literal or a make without a hint grows.
func sizedLocals(fd *ast.FuncDecl) map[string]bool {
	sized := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, rhs := range assign.Rhs {
			call, isCall := rhs.(*ast.CallExpr)
			fn, isIdent := callName(call, isCall)
			if isIdent && fn == "make" && madeWithSize(call.Args) {
				sized[exprKey(assign.Lhs[i])] = true
			}
		}
		return true
	})
	return sized
}

// madeWithSize reports make arguments that give a capacity, or a length that
// is not the literal 0: make([]T, 0) still grows on every append.
func madeWithSize(args []ast.Expr) bool {
	switch len(args) {
	case 3:
		return true
	case 2:
		lit, ok := args[1].(*ast.BasicLit)
		return !ok || lit.Value != "0"
	}
	return false
}

func callName(call *ast.CallExpr, ok bool) (string, bool) {
	if !ok {
		return "", false
	}
	id, isIdent := call.Fun.(*ast.Ident)
	if !isIdent {
		return "", false
	}
	return id.Name, true
}
