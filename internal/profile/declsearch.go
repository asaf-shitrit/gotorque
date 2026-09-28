package profile

import (
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Symbol is a runtime function name split into the parts a declaration
// spells: go.starlark.net/starlark.(*Function).CallInternal is package
// go.starlark.net/starlark, receiver Function, function CallInternal.
type Symbol struct {
	Package  string
	Receiver string
	Func     string
}

// ParseSymbol splits a sampler or pprof function name. Closure suffixes must
// already be trimmed; a method value's -fm wrapper and generic type
// arguments ([...]) are removed here.
func ParseSymbol(name string) Symbol {
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return Symbol{Func: name}
	}
	rest := stripTypeArgs(strings.TrimSuffix(name[slash+2+dot:], "-fm"))
	parts := strings.Split(rest, ".")
	sym := Symbol{Package: name[:slash+1+dot], Func: parts[len(parts)-1]}
	if len(parts) == 2 {
		sym.Receiver = strings.TrimSuffix(strings.TrimPrefix(parts[0], "(*"), ")")
	}
	return sym
}

func stripTypeArgs(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// declPattern matches the declaration of sym: a method only on its own
// receiver type, a function only without a receiver. Matching the bare name
// sent starlark's Int.get to whichever file declared any get first.
func declPattern(sym Symbol) *regexp.Regexp {
	recv := ""
	if sym.Receiver != "" {
		recv = `\(\s*(?:\w+\s+)?\*?` + regexp.QuoteMeta(sym.Receiver) + `(?:\[[^\]]*\])?\s*\)\s*`
	}
	return regexp.MustCompile(`(?m)^func\s+` + recv + regexp.QuoteMeta(sym.Func) + `(?:\[.*\])?\(`)
}

// FindDeclaration locates sym's declaration under root, never in a test file.
// When the package maps to a directory of the root module, only that
// directory is searched. Otherwise the whole repository is, and the match
// must be unique: a bare name that several packages declare has no single
// source position, and guessing one points the agents at the wrong code.
func FindDeclaration(root string, sym Symbol) (string, int, bool) {
	if sym.Func == "" {
		return "", 0, false
	}
	decl := declPattern(sym)
	if dir, ok := packageDir(root, sym.Package); ok {
		return firstInDir(root, dir, decl)
	}
	s := declSearch{root: root, decl: decl, mainOnly: sym.Package == "main"}
	_ = filepath.WalkDir(root, s.visit)
	if len(s.matches) != 1 {
		return "", 0, false
	}
	return s.matches[0].path, s.matches[0].line, true
}

// FindDeclarationInDir searches one package directory for sym, for a
// package whose import path names no directory: a sampled main.* frame
// belongs to the binary that was built, wherever its main package lives.
func FindDeclarationInDir(root, dir string, sym Symbol) (string, int, bool) {
	if sym.Func == "" {
		return "", 0, false
	}
	return firstInDir(root, dir, declPattern(sym))
}

// packageDir maps an import path inside the root module to its directory.
func packageDir(root, pkg string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || pkg == "" {
		return "", false
	}
	module := modulePath(data)
	rel, ok := strings.CutPrefix(pkg, module)
	if module == "" || !ok || (rel != "" && rel[0] != '/') {
		return "", false
	}
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

func modulePath(gomod []byte) string {
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

func isSourceFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// builds reports whether the file is compiled for this platform. A package
// can declare a function once per build-tag variant (int_posix64.go,
// int_generic.go); the one this machine builds is the one that was sampled.
func builds(path string) bool {
	ok, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
	return err == nil && ok
}

// firstInDir searches one package directory in name order, among the files
// this platform builds.
func firstInDir(root, dir string, decl *regexp.Regexp) (string, int, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && isSourceFile(entry.Name()) && builds(filepath.Join(dir, entry.Name())) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if m, ok := matchDecl(root, filepath.Join(dir, name), decl); ok {
			return m.path, m.line, true
		}
	}
	return "", 0, false
}

type declMatch struct {
	path string
	line int
}

func matchDecl(root, path string, decl *regexp.Regexp) (declMatch, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return declMatch{}, false
	}
	loc := decl.FindIndex(data)
	if loc == nil {
		return declMatch{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return declMatch{path: filepath.ToSlash(rel), line: 1 + strings.Count(string(data[:loc[0]]), "\n")}, true
}

var packageMain = regexp.MustCompile(`(?m)^package main\b`)

// declSearch walks the repository with a bounded file count, collecting at
// most two matches: one is an answer, two is an ambiguity.
type declSearch struct {
	root     string
	decl     *regexp.Regexp
	mainOnly bool
	matches  []declMatch
	files    int
}

func (s *declSearch) visit(path string, d os.DirEntry, err error) error {
	// WalkDir reports errors only for directories it cannot read; the search
	// is best effort, so such a directory is skipped.
	if err != nil {
		return filepath.SkipDir
	}
	if d.IsDir() {
		return skipIgnoredDir(d.Name())
	}
	if !isSourceFile(path) || !builds(path) {
		return nil
	}
	s.files++
	if s.files > 2000 || len(s.matches) > 1 {
		return filepath.SkipAll
	}
	if m, ok := matchDecl(s.root, path, s.decl); ok && s.inPackage(path) {
		s.matches = append(s.matches, m)
	}
	return nil
}

// inPackage keeps a main.* symbol out of library packages that happen to
// declare the same name.
func (s *declSearch) inPackage(path string) bool {
	if !s.mainOnly {
		return true
	}
	data, err := os.ReadFile(path)
	return err == nil && packageMain.Match(data)
}
