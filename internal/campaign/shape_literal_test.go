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

// TestLiteralDriftReplaysRecordedPatches runs the rule over every patch the
// live campaigns recorded: it may fire on the retyped fzf literal and on
// nothing else, since a false rejection costs a good candidate (ADR 0017).
func TestLiteralDriftReplaysRecordedPatches(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	patches, _ := filepath.Glob(filepath.Join(home, "projects", "gotorque-work", "campaigns", "*", "patches", "*.diff"))
	if len(patches) == 0 {
		t.Skip("no recorded campaigns at ~/projects/gotorque-work; skipping the replay gate")
	}
	const fzf = "b6630f4bf77b1f73f9981bdc"
	sawFzf, firedFzf := false, false
	for _, path := range patches {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		sawFzf = sawFzf || strings.Contains(path, fzf)
		for name, change := range parseChanges(data) {
			if err := checkLiteralDrift(name, change); err != nil {
				require.Contains(t, path, fzf, "unexpected rejection: %v", err)
				firedFzf = true
			}
		}
	}
	require.Equal(t, sawFzf, firedFzf, "the retyped fzf literal must be rejected whenever its patch is on record")
}
