package workload

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeSource(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBoolFlagsFoldsAliasesByTheVariableTheySet mirrors gron: stdlib flag,
// short and long spellings bound to one variable, and non-bool flags ignored.
func TestBoolFlagsFoldsAliasesByTheVariableTheySet(t *testing.T) {
	dir := writeSource(t, map[string]string{"main.go": `package main

import "flag"

var streamFlag, ungronFlag bool
var proxyURL string

func main() {
	flag.BoolVar(&ungronFlag, "ungron", false, "")
	flag.BoolVar(&ungronFlag, "u", false, "")
	flag.BoolVar(&streamFlag, "s", false, "")
	flag.BoolVar(&streamFlag, "stream", false, "")
	flag.StringVar(&proxyURL, "x", "", "")
	verbose := flag.Bool("verbose", false, "")
	_ = verbose
}
`, "main_test.go": `package main

import "flag"

func init() { flag.Bool("test-only", false, "") }
`})
	flags, err := BoolFlags(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Flag{{Name: "--ungron", Aliases: []string{"-u"}}, {Name: "--stream", Aliases: []string{"-s"}}, {Name: "--verbose"}}
	if !reflect.DeepEqual(flags, want) {
		t.Errorf("flags = %+v, want %+v", flags, want)
	}
}

// TestBoolFlagsReadsPflagAndGoFlagsForms covers cobra/pflag P forms on any
// receiver and gojq's go-flags struct tags.
func TestBoolFlagsReadsPflagAndGoFlagsForms(t *testing.T) {
	dir := writeSource(t, map[string]string{"cli.go": `package cli

type flagopts struct {
	InputStream bool   ` + "`long:\"stream\" description:\"parse input in stream fashion\"`" + `
	Compact     bool   ` + "`short:\"c\" long:\"compact-output\" description:\"compact output\"`" + `
	Indent      int    ` + "`long:\"indent\"`" + `
	NoTag       bool
}

func register(cmd interface{ Flags() fs }) {
	var raw bool
	cmd.Flags().BoolVarP(&raw, "raw-output", "r", false, "output raw strings")
	cmd.Flags().BoolP("null-input", "n", false, "use null as input")
}
`})
	flags, err := BoolFlags(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Flag{
		{Name: "--stream"},
		{Name: "--compact-output", Aliases: []string{"-c"}},
		{Name: "--raw-output", Aliases: []string{"-r"}},
		{Name: "--null-input", Aliases: []string{"-n"}},
	}
	if !reflect.DeepEqual(flags, want) {
		t.Errorf("flags = %+v, want %+v", flags, want)
	}
}

func TestBoolFlagsReportsUnparseableSource(t *testing.T) {
	dir := writeSource(t, map[string]string{"broken.go": "package main\nfunc {"})
	if _, err := BoolFlags(dir); err == nil {
		t.Error("parsed broken source")
	}
	if flags, err := BoolFlags(t.TempDir()); err != nil || len(flags) != 0 {
		t.Errorf("empty dir: %v, %v", flags, err)
	}
}
