package campaign

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/jev"
)

// fileChange is what a patch did to one file, read from `git diff -U0` of the
// applied worktree: the new-side lines it touched, and the text it added and
// removed.
type fileChange struct {
	lines   map[int]bool
	added   []string
	removed []string
}

var newSideHunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseChanges reads zero-context git diff output. A hunk that only deletes
// has no new-side lines, so it is anchored at the line it follows: that line
// sits in the same function as what was deleted, unless the deletion was a
// whole declaration.
func parseChanges(diff []byte) map[string]*fileChange {
	changes := map[string]*fileChange{}
	var current *fileChange
	for line := range strings.SplitSeq(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// Each file's header starts here, so its "--- a/" line is never
			// read as a line removed from the file before it.
			current = nil
		case strings.HasPrefix(line, "+++ "):
			current = nil
			if name, ok := strings.CutPrefix(line, "+++ b/"); ok {
				current = &fileChange{lines: map[int]bool{}}
				changes[name] = current
			}
		case current == nil:
		case strings.HasPrefix(line, "@@"):
			markHunk(current, line)
		case strings.HasPrefix(line, "+"):
			current.added = append(current.added, line[1:])
		case strings.HasPrefix(line, "-"):
			current.removed = append(current.removed, line[1:])
		}
	}
	return changes
}

func markHunk(change *fileChange, header string) {
	m := newSideHunk.FindStringSubmatch(header)
	if m == nil {
		return
	}
	start, _ := strconv.Atoi(m[1])
	count := 1
	if m[2] != "" {
		count, _ = strconv.Atoi(m[2])
	}
	if count == 0 {
		change.lines[max(start, 1)] = true
		return
	}
	for l := start; l < start+count; l++ {
		change.lines[l] = true
	}
}

// checkShape judges an applied patch before it is built. It rejects what a
// compiler would reject later and what no build could catch: a patch that
// does not parse, one that calls a standard package it never imports, one
// that edits a function other than the target it was written for, and one
// that does not carry the mechanism of the remedy it was asked to apply.
//
// On a live gojq campaign the optimizer wrapped printValues in a
// bufio.Writer, exactly the fix the target asked for, and forgot the bufio
// import: the build failed, the target counted as tried, and the next cycle
// moved on. Named here, the reason goes back to the optimizer with the target
// still open.
func checkShape(worktree string, diff []byte, target *agents.Target) error {
	changes := parseChanges(diff)
	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if err := checkFile(worktree, name, changes[name], target); err != nil {
			return err
		}
	}
	return checkRemedy(worktree, changes, target)
}

func checkFile(worktree, name string, change *fileChange, target *agents.Target) error {
	full := filepath.Join(worktree, filepath.FromSlash(name))
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, full, data, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("%s does not parse after the patch: %w", name, relativeError(err, worktree))
	}
	if err := checkImports(fset, filepath.Dir(full), file, change); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if target == nil {
		return nil
	}
	if target.Functions != "" {
		return checkMultiConfined(fset, file, change, name, *target)
	}
	if targetPath(target.Location) != name {
		return nil
	}
	if err := checkConfined(fset, file, change, target.Function); err != nil {
		return err
	}
	return checkBypass(file, target.Function)
}

// checkMultiConfined is checkConfined's counterpart for a throwaway_result
// target's function set (ADR 0027): the callee plus its consuming callers,
// which commonly live in files other than the callee's own, since a callee's
// callers are a package-wide search, not a single-file one. It applies the
// same confinement checkConfined applies to the target's own file to every
// file in the callee's package directory; a file outside that directory is
// not checked at all, exactly like the single-function path, which never
// confines a file other than the target's own.
func checkMultiConfined(fset *token.FileSet, file *ast.File, change *fileChange, name string, target agents.Target) error {
	calleeDir := path.Dir(targetPath(target.Location))
	if path.Dir(name) != calleeDir {
		return nil
	}
	allowed := map[string]bool{target.Function: true}
	var names []string
	for _, f := range agents.DecodeFunctionSet(target.Functions) {
		allowed[f.Name] = true
		names = append(names, f.Name)
	}
	var others []string
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || allowed[funcName(fd)] {
			continue
		}
		first, last := fset.Position(fd.Pos()).Line, fset.Position(fd.End()).Line
		if touched(change, first, last) && !whollyNewDecl(fset, fd, change) {
			others = append(others, funcName(fd))
		}
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("the patch edits %s, outside the throwaway_result set (%s, %s); change only those functions or add a wholly new one",
		strings.Join(others, ", "), target.Function, strings.Join(names, ", "))
}

// whollyNewDecl reports whether fd's declaration did not exist under this
// name before the patch: its own signature line is itself changed content
// (present in change.lines, the zero-context diff's new-side line numbers),
// and no removed line elsewhere in the file declared a function of the same
// name (which would mean this name existed and was rewritten, not added).
//
// checkConfined's own added() instead requires every line of the
// declaration to be added, which a throwaway_result remedy routinely fails:
// dasel's fix1 adds unpackKindsValue by moving UnpackKinds' unchanged loop
// body under a new name and signature, so most of the new declaration's
// lines are identical, unmarked context in a zero-context diff, not added
// lines. Checking only the signature line is enough to tell "this name is
// new" from "this name's body was edited in place", which is what
// checkMultiConfined actually needs to know.
func whollyNewDecl(fset *token.FileSet, fd *ast.FuncDecl, change *fileChange) bool {
	first := fset.Position(fd.Pos()).Line
	if !change.lines[first] {
		return false
	}
	return !slices.ContainsFunc(change.removed, func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "func ") && strings.Contains(line, " "+fd.Name.Name+"(")
	})
}

// checkBypass rejects a target function that wraps a writer in a
// bufio.Writer and still writes to the writer directly: the direct writes
// land before the buffered ones and the output is reordered. On a live gojq
// campaign a patch buffered printValues' values and left its newline writes
// on cli.outStream; it built, and the test gate caught it only after a full
// build and test run, with the target then counted as tried.
func checkBypass(file *ast.File, function string) error {
	fd := findFunc(file, function)
	if fd == nil || fd.Body == nil {
		return nil
	}
	wrapped := bufferedWriters(fd.Body)
	if len(wrapped) == 0 {
		return nil
	}
	var bypassed []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if dst, ok := ioDestination(call); ok && wrapped[exprKey(dst)] && !slices.Contains(bypassed, exprKey(dst)) {
			bypassed = append(bypassed, exprKey(dst))
		}
		return true
	})
	if len(bypassed) == 0 {
		return nil
	}
	return fmt.Errorf("%s still writes to %s directly after wrapping it in a bufio.Writer, so those writes would land before the buffered ones; route every write through the writer", function, strings.Join(bypassed, ", "))
}

// bufferedWriters names every writer the body wraps in a bufio.Writer.
func bufferedWriters(body *ast.BlockStmt) map[string]bool {
	wrapped := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || constructors[typeName(call.Fun)] != "bufio.Writer" || len(call.Args) == 0 {
			return true
		}
		if key := exprKey(call.Args[0]); key != "" {
			wrapped[key] = true
		}
		return true
	})
	return wrapped
}

func relativeError(err error, worktree string) error {
	return errors.New(strings.ReplaceAll(err.Error(), worktree+string(filepath.Separator), ""))
}

func targetPath(location string) string {
	file, _, _ := strings.Cut(location, ":")
	return filepath.ToSlash(file)
}

// standardPackages are the packages a performance patch reaches for. A
// selector on one of these names, on a line the patch changed, in a file that
// neither imports the package nor declares the name, can only be a missing
// import.
var standardPackages = map[string]bool{
	"bufio": true, "bytes": true, "cmp": true, "errors": true, "fmt": true,
	"io": true, "maps": true, "math": true, "os": true, "slices": true,
	"sort": true, "strconv": true, "strings": true, "sync": true,
	"unicode": true, "utf8": true,
}

func checkImports(fset *token.FileSet, dir string, file *ast.File, change *fileChange) error {
	imported := importedNames(file)
	var missing []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && standardPackages[id.Name] && !imported[id.Name] && change.lines[fset.Position(id.Pos()).Line] && !slices.Contains(missing, id.Name) {
			missing = append(missing, id.Name)
		}
		return true
	})
	declared := declaredNames(file)
	missing = slices.DeleteFunc(missing, func(name string) bool { return declared[name] || declaredInPackage(dir, file, name) })
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("the patch uses %s without importing it; add the import in the same patch", strings.Join(missing, ", "))
}

var majorVersion = regexp.MustCompile(`^v\d+$`)

func importedNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		if spec.Name != nil {
			names[spec.Name.Name] = true
			continue
		}
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		base := path.Base(p)
		if majorVersion.MatchString(base) {
			base = path.Base(path.Dir(p))
		}
		names[base] = true
	}
	return names
}

// declaredNames is every name the file declares at any depth, so a local
// variable that happens to be called `bytes` is not taken for the package.
func declaredNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		for _, id := range declaredBy(n) {
			if id != nil {
				names[id.Name] = true
			}
		}
		return true
	})
	return names
}

func declaredBy(n ast.Node) []*ast.Ident {
	switch d := n.(type) {
	case *ast.AssignStmt:
		if d.Tok != token.DEFINE {
			return nil
		}
		ids := make([]*ast.Ident, 0, len(d.Lhs))
		for _, lhs := range d.Lhs {
			id, _ := lhs.(*ast.Ident)
			ids = append(ids, id)
		}
		return ids
	case *ast.RangeStmt:
		if d.Tok != token.DEFINE {
			return nil
		}
		k, _ := d.Key.(*ast.Ident)
		v, _ := d.Value.(*ast.Ident)
		return []*ast.Ident{k, v}
	case *ast.ValueSpec:
		return d.Names
	case *ast.Field:
		return d.Names
	case *ast.TypeSpec:
		return []*ast.Ident{d.Name}
	case *ast.FuncDecl:
		return []*ast.Ident{d.Name}
	}
	return nil
}

// declaredInPackage reports whether another file of the package declares name
// at top level, which the file itself cannot show.
func declaredInPackage(dir string, self *ast.File, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil || f.Name.Name != self.Name.Name {
			continue
		}
		if topLevelDeclares(f, name) {
			return true
		}
	}
	return false
}

func topLevelDeclares(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == name {
				return true
			}
		case *ast.GenDecl:
			if genDeclares(d, name) {
				return true
			}
		}
	}
	return false
}

func genDeclares(d *ast.GenDecl, name string) bool {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if slices.ContainsFunc(s.Names, func(id *ast.Ident) bool { return id.Name == name }) {
				return true
			}
		case *ast.TypeSpec:
			if s.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// checkConfined rejects a change inside any existing function other than the
// target. Imports, package-level declarations and wholly new functions are
// allowed: a remedy may need a helper or a reusable buffer next to the
// function it fixes. A target the file no longer declares is not judged here;
// the build and the test gate still are.
func checkConfined(fset *token.FileSet, file *ast.File, change *fileChange, function string) error {
	var others []string
	found := false
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if funcName(fd) == function {
			found = true
			continue
		}
		first, last := fset.Position(fd.Pos()).Line, fset.Position(fd.End()).Line
		if touched(change, first, last) && !added(change, fd, first, last) {
			others = append(others, funcName(fd))
		}
	}
	if !found || len(others) == 0 {
		return nil
	}
	return fmt.Errorf("the patch edits %s, but its target is %s; change only the target function", strings.Join(others, ", "), function)
}

func touched(change *fileChange, first, last int) bool {
	for l := first; l <= last; l++ {
		if change.lines[l] {
			return true
		}
	}
	return false
}

// added reports whether the function is new: every line of it was added, and
// no removed line declared a function of the same name. The second half is
// what tells a new helper from a one-line function rewritten in place.
func added(change *fileChange, fd *ast.FuncDecl, first, last int) bool {
	for l := first; l <= last; l++ {
		if !change.lines[l] {
			return false
		}
	}
	return !slices.ContainsFunc(change.removed, func(line string) bool {
		return strings.HasPrefix(line, "func ") && strings.Contains(line, " "+fd.Name.Name+"(")
	})
}

// remedyShapes are the mechanisms whose absence rules a patch out, for the
// causes whose remedy has exactly one recognisable shape. The other causes
// (allocation, fast path, superlinear work, redundant work) are fixed in too
// many ways to name one, so they are not judged here.
var remedyShapes = map[jev.Cause]struct {
	anyOf   []string
	missing string
}{
	jev.CauseUnbufferedIO: {[]string{"bufio."}, "the target's remedy is buffered I/O, but the patch adds no bufio reader or writer"},
	jev.CauseStringBuild:  {[]string{"strings.Builder", "bytes.Buffer", "strconv.", "append(", ".Grow("}, "the target's remedy is building strings with strings.Builder, strconv or appends, and the patch adds none of them"},
	jev.CausePrealloc:     {[]string{"make(", ".Grow("}, "the target's remedy is sizing a container up front, and the patch adds no make or Grow call"},
}

// checkRemedy asks the added lines for the mechanism the target's cause names.
func checkRemedy(worktree string, changes map[string]*fileChange, target *agents.Target) error {
	if target == nil {
		return nil
	}
	if target.Functions != "" {
		return checkSwitchedCallers(worktree, changes, *target)
	}
	if target.FixKind == string(jev.KindDropFmt) {
		return checkDroppedFmt(changes)
	}
	return checkCauseShape(worktree, changes, target)
}

// checkCauseShape asks the added lines for the mechanism remedyShapes names
// for the target's cause, and a bufio.Writer for its flush.
func checkCauseShape(worktree string, changes map[string]*fileChange, target *agents.Target) error {
	shape, ok := remedyShapes[jev.Cause(target.Cause)]
	if !ok {
		return nil
	}
	var added strings.Builder
	for _, c := range changes {
		for _, line := range c.added {
			added.WriteString(line)
			added.WriteByte('\n')
		}
	}
	text := added.String()
	if !containsAny(text, shape.anyOf...) {
		return errors.New(shape.missing)
	}
	if strings.Contains(text, "bufio.NewWriter") && !strings.Contains(text, ".Flush(") && !fileFlushes(worktree, target.Location) {
		return errors.New("the patch adds a bufio.Writer but never flushes it, so buffered output would be lost")
	}
	return nil
}

// checkSwitchedCallers holds a throwaway_result patch to its remedy: the
// waste is in the callers that drop the callee's fresh allocation, so a patch
// that edits none of them has not applied it. On the first live dasel
// campaign with these targets the optimizer rewrote only UnpackKinds'
// internals, measured 0%, and the target counted as tried; rejected here
// before the build, the reason goes back with the target still open.
func checkSwitchedCallers(worktree string, changes map[string]*fileChange, target agents.Target) error {
	callers := agents.DecodeFunctionSet(target.Functions)
	for _, ref := range callers {
		if callerTouched(worktree, changes, ref) {
			return nil
		}
	}
	names := make([]string, 0, len(callers))
	for _, ref := range callers {
		names = append(names, ref.Name)
	}
	return fmt.Errorf("the target's remedy is switching %s's callers to a non-allocating variant, but the patch changes none of %s", target.Function, strings.Join(names, ", "))
}

// callerTouched reports whether the patch changed ref's declaration, read from
// the patched file. A caller the parser cannot find counts as untouched.
func callerTouched(worktree string, changes map[string]*fileChange, ref agents.FunctionRef) bool {
	name := targetPath(ref.Location)
	change, ok := changes[name]
	if !ok {
		return false
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(worktree, filepath.FromSlash(name)), nil, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	fd := findFuncDecl(file, ref.Name)
	if fd == nil {
		return false
	}
	return touched(change, fset.Position(fd.Pos()).Line, fset.Position(fd.End()).Line)
}

// checkDroppedFmt holds the drop-fmt remedy to its own shape: it replaces a
// fmt call, so it removes one or adds strconv, and may add none of the
// string-building mechanisms remedyShapes asks of the cause in general.
func checkDroppedFmt(changes map[string]*fileChange) error {
	for _, c := range changes {
		if slices.ContainsFunc(c.removed, func(l string) bool { return strings.Contains(l, "fmt.") }) ||
			slices.ContainsFunc(c.added, func(l string) bool { return strings.Contains(l, "strconv.") }) {
			return nil
		}
	}
	return errors.New("the target's remedy is replacing fmt formatting, but the patch removes no fmt call and adds no strconv call")
}

func containsAny(text string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(s string) bool { return strings.Contains(text, s) })
}

// fileFlushes reports whether the target's file flushes something anywhere,
// for a patch that adds a writer and relies on an existing Flush call.
func fileFlushes(worktree, location string) bool {
	data, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(targetPath(location))))
	return err == nil && strings.Contains(string(data), ".Flush(")
}
