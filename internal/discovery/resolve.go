package discovery

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
	"github.com/asaf-shitrit/gotorque/internal/toolchain"
)

// locator turns function names into repository-relative source positions.
type locator struct {
	repository string
	build      manifest.BuildTarget
	tc         *toolchain.Toolchain
}

// resolve annotates hot function names with repository-relative source
// positions. Preferred source is `go tool pprof -list` over the benchmark
// profile (exact file and sampled line); the fallback searches the repository
// for the declaration. Unresolvable functions keep their bare names so
// downstream consumers never lose entries.
func (l locator) resolve(ctx context.Context, cpuProfile string, names []string) []string {
	locations := make([]string, 0, hotFunctionBudget)
	for _, name := range names {
		if len(locations) == hotFunctionBudget {
			break
		}
		// A value method and the pointer wrapper Go generates for it are two
		// symbols with one declaration: gron's hot list named statements.go:312
		// twice, spending a slot of the budget on a repeat.
		if loc := l.location(ctx, cpuProfile, name); !slices.Contains(locations, loc) {
			locations = append(locations, loc)
		}
	}
	return locations
}

func (l locator) location(ctx context.Context, cpuProfile, name string) string {
	// pprof's top listing marks inlined frames "name (inline)", a display
	// suffix no symbol or declaration carries: chroma's lexers.Get stayed a
	// bare name on every campaign because of it.
	name = strings.TrimSuffix(name, " (inline)")
	if loc, ok := l.fromProfile(ctx, name, cpuProfile); ok {
		return loc
	}
	if loc, ok := l.fromRepo(name); ok {
		return loc
	}
	return name
}

func (l locator) fromProfile(ctx context.Context, name, cpuProfile string) (string, bool) {
	if cpuProfile == "" {
		return "", false
	}
	// -list takes a regular expression. Unquoted, a method name such as
	// pkg.(*Function).CallInternal fails to parse, and unanchored, any name
	// matches every symbol containing it.
	result, err := l.tc.PprofList(ctx, "^"+regexp.QuoteMeta(name)+"$", cpuProfile)
	if err != nil {
		return "", false
	}
	path, line, ok := profile.ParsePprofList(string(result.Stdout))
	if !ok {
		return "", false
	}
	path, ok = repoRelative(l.repository, path)
	// A benchmark profile runs test helpers too (scc's filereader_test.go
	// reached the hot list), and a patch may not edit a test file, so the
	// location says nothing a candidate could act on.
	if !ok || strings.HasSuffix(path, "_test.go") {
		return "", false
	}
	return profile.HotLocation{Function: name, Path: path, Line: line}.Location(), true
}

func (l locator) fromRepo(name string) (string, bool) {
	if strings.Contains(name, ":") {
		return "", false
	}
	sym := profile.ParseSymbol(enclosingFunction(name))
	path, line, ok := l.findMain(sym)
	if !ok {
		path, line, ok = profile.FindDeclaration(l.repository, sym)
	}
	if !ok {
		return "", false
	}
	return profile.HotLocation{Function: name, Path: path, Line: line}.Location(), true
}

// findMain resolves a main.* frame in the target's own main package. A
// repository with several commands declares the same names in each, and a
// repository-wide search refuses them as ambiguous.
func (l locator) findMain(sym profile.Symbol) (string, int, bool) {
	if sym.Package != "main" || !strings.HasPrefix(l.build.Package, ".") {
		return "", 0, false
	}
	dir := filepath.Join(l.repository, l.build.Directory, l.build.Package)
	return profile.FindDeclarationInDir(l.repository, dir, sym)
}
