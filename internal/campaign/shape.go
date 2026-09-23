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
	if target == nil || targetPath(target.Location) != name {
		return nil
	}
	return checkConfined(fset, file, change, target.Function)
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

func containsAny(text string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(s string) bool { return strings.Contains(text, s) })
}

// fileFlushes reports whether the target's file flushes something anywhere,
// for a patch that adds a writer and relies on an existing Flush call.
func fileFlushes(worktree, location string) bool {
	data, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(targetPath(location))))
	return err == nil && strings.Contains(string(data), ".Flush(")
}
