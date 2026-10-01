package campaign

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/jev"
	"github.com/asaf-shitrit/gotorque/internal/orchestrator"
)

// causeUnbufferedWrites is the code-derived cause ADR 0032 adds: the target
// sample's call stacks show a hot function spending most of its time in write
// system calls with no bufio frame on the path. It is distinct from Jev's
// unbuffered_io, which judges source text; here the evidence is measured.
const causeUnbufferedWrites = "unbuffered_writes"

// addUnbufferedWriteTargets puts a target for every unbuffered-write site
// first, ahead of the throwaway_result and Jev-ranked targets: like
// throwaway_result (ADR 0027) the evidence is deterministic, and unlike Jev
// it sees through a closure. On fzf the Printer closure's enclosing function
// is a 2.4 KB options constructor, and Jev, shown that, put unbuffered I/O at
// 0.04. A Jev unbuffered_io target at the same location is dropped, since it
// would try the same remedy twice. It never touches a siteVerdict.
func addUnbufferedWriteTargets(repo string, writes []orchestrator.UnbufferedWrite, result *agents.AnalystResult) {
	targets := make([]agents.Target, 0, len(writes))
	var paths []agents.HotPath
	for _, w := range writes {
		t, ok := unbufferedWriteTarget(repo, w)
		if !ok {
			continue
		}
		targets = append(targets, t)
		paths = append(paths, agents.HotPath{Location: t.Location, Evidence: "code-derived: unbuffered_writes"})
		for _, c := range t.Callers {
			paths = append(paths, agents.HotPath{Location: c.Location, Evidence: "code-derived: unbuffered_writes caller"})
		}
	}
	if len(targets) == 0 {
		return
	}
	kept := make([]agents.Target, 0, len(result.Targets))
	for _, t := range result.Targets {
		if t.Cause == string(jev.CauseUnbufferedIO) && writesCover(targets, t.Location) {
			continue
		}
		kept = append(kept, t)
	}
	result.Targets = slices.Concat(targets, kept)
	remedies := make([]string, 0, len(targets))
	for _, t := range targets {
		remedies = append(remedies, t.Remedy)
	}
	result.CandidateHypotheses = append(remedies, result.CandidateHypotheses...)
	result.HotPaths = append(result.HotPaths, paths...)
}

func writesCover(targets []agents.Target, location string) bool {
	for _, t := range targets {
		if t.Location == location {
			return true
		}
	}
	return false
}

// unbufferedWriteTarget names the writing function and, as a function set,
// the callers small enough to send whole; a caller over the classification
// limit is still named in the remedy, since the loop over records is often
// there, but the patch is not invited to rewrite it.
func unbufferedWriteTarget(repo string, w orchestrator.UnbufferedWrite) (agents.Target, bool) {
	fn, ok := functionNamed(repo, w.Location)
	if !ok {
		return agents.Target{}, false
	}
	var refs []agents.FunctionRef
	var named []string
	for _, loc := range w.Callers {
		caller, ok := functionNamed(repo, loc)
		if !ok || caller.Name == fn.Name {
			continue
		}
		named = append(named, caller.Name)
		if len(caller.Source) <= maxCauseSourceBytes {
			refs = append(refs, agents.FunctionRef{Name: caller.Name, Location: loc})
		}
	}
	t := agents.Target{
		Location: w.Location,
		Function: fn.Name,
		Cause:    causeUnbufferedWrites,
		Remedy:   unbufferedWriteRemedy(fn.Name, w.Share, named),
	}
	if len(refs) > 0 {
		t.Kind, t.Callers = agents.TargetFunctionSet, refs
	}
	return t, true
}

func unbufferedWriteRemedy(function string, share float64, callers []string) string {
	remedy := fmt.Sprintf("The target sample caught %s spending %d%% of its time in write system calls "+
		"with no bufio frame between it and the kernel: every record it emits is its own write.", function, int(math.Round(100*share)))
	if len(callers) > 0 {
		remedy += " It was called on those paths by " + strings.Join(callers, ", ") + "."
	}
	return remedy + " Route the output through one bufio.Writer over the same destination and flush it " +
		"exactly once, after the last record and before the program can exit or hand the stream to anything " +
		"else, so the bytes written and their order stay identical."
}

// functionNamed reads the whole declaration at a hot-list location, without
// the classification size limit: a caller's size decides only whether it
// joins the function set.
func functionNamed(repo, location string) (hotFunction, bool) {
	path, line, ok := parseLocation(location)
	if !ok || line == 0 || !strings.HasSuffix(path, ".go") {
		return hotFunction{}, false
	}
	fn, _, err := declarationAt(filepath.Join(repo, path), line)
	if err != nil {
		return hotFunction{}, false
	}
	return fn, true
}
