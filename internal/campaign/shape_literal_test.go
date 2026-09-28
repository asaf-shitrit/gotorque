package campaign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func literalChange(removed, added string) *fileChange {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	return &fileChange{lines: map[int]bool{}, removed: split(removed), added: split(added)}
}

// TestLiteralDriftRejectsARetypedLiteral is the live fzf candidate, cut down:
// the Printer change the remedy asked for came with a dropped Unicode field
// and ClearOnExit and WalkerRoot switched off.
func TestLiteralDriftRejectsARetypedLiteral(t *testing.T) {
	change := literalChange(
		"\t\tPrinter:      func(str string) { fmt.Println(str) },\n\t\tUnicode:      true,\n\t\tClearOnExit:  true,\n\t\tWalkerRoot:   []string{\".\"},\n\t\tTabstop:      8,\n",
		"\t\tPrinter:      func(str string) { fmt.Fprintln(stdout, str) },\n\t\tClearOnExit:  false,\n\t\tWalkerRoot:   []string{},\n\t\tTabstop:      8,\n\t\tWithShell:    \"\",\n",
	)
	err := checkLiteralDrift("src/options.go", change)
	require.ErrorContains(t, err, "src/options.go: the patch rewrites a struct literal and ClearOnExit changes from true to false, Unicode is dropped, WalkerRoot changes from []string{\".\"} to []string{}")
	require.NotContains(t, err.Error(), "Printer")
	require.NotContains(t, err.Error(), "Tabstop")
}

func TestLiteralDriftAllowsWhatOptimizationsDo(t *testing.T) {
	cases := map[string]*fileChange{
		"the remedy's own field": literalChange("\tPrinter: func(s string) { fmt.Println(s) },\n", "\tPrinter: func(s string) { fmt.Fprintln(w, s) },\n"),
		"a literal hoisted out of a loop": literalChange(
			"\t\tv := &T{Name: n, Size: 4}\n\t\tName: n,\n", "\tName: n,\n\tSize: 4,\n"),
		"a literal replaced by assignments": literalChange("\t\tName: n,\n\t\tSize: 4,\n", "\tv.Name = n\n\tv.Size = 4\n"),
		"a buffer resized":                  literalChange("\tSize: 4096,\n", "\tSize: 65536,\n"),
		"a value computed differently":      literalChange("\tTitle: title,\n", "\tTitle: getTitle(input),\n"),
		"case clauses and labels":           literalChange("\tcase x:\n\touter:\n\tdefault:\n\t// Note: old\n", "\tName: n,\n"),
		"not Go":                            literalChange("\tName: n\n", "\tOther: m\n"),
	}
	for name, change := range cases {
		file := "a.go"
		if name == "not Go" {
			file = "a.yaml"
		}
		require.NoError(t, checkLiteralDrift(file, change), name)
	}
}

// reviewedDriftCatches are recorded patches the rule rejects, each read and
// confirmed to change behaviour beyond its remedy. A rejection of any other
// recorded patch fails the replay until it is reviewed and listed here: a
// false rejection costs a good candidate (ADR 0017).
var reviewedDriftCatches = map[string]string{
	"b6630f4bf77b1f73f9981bdc": "live2-fzf: Options literal retyped; Unicode dropped, ClearOnExit switched off",
	"02810e0fd72d07d033542ec2": "live7-hclfmt: TokenQuotedNewline and TokenInvalid diagnostics deleted",
}

// TestLiteralDriftReplaysRecordedPatches runs the rule over every patch the
// live campaigns recorded: it must reject each reviewed catch that is on
// record and nothing else.
func TestLiteralDriftReplaysRecordedPatches(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	patches, _ := filepath.Glob(filepath.Join(home, "projects", "gotorque-work", "campaigns", "*", "patches", "*.diff"))
	if len(patches) == 0 {
		t.Skip("no recorded campaigns at ~/projects/gotorque-work; skipping the replay gate")
	}
	for _, path := range patches {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		id := strings.TrimSuffix(filepath.Base(path), ".diff")
		fired := false
		for name, change := range parseChanges(data) {
			if err := checkLiteralDrift(name, change); err != nil {
				_, reviewed := reviewedDriftCatches[id]
				require.True(t, reviewed, "unreviewed rejection of %s: %v", path, err)
				fired = true
			}
		}
		if note, reviewed := reviewedDriftCatches[id]; reviewed {
			require.True(t, fired, "reviewed catch no longer rejected: %s (%s)", path, note)
		}
	}
}
