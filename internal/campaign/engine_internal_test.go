package campaign

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"example.com/gotorque/internal/profile"
)

func TestHotFunctionNamesDropsNonActionableFrames(t *testing.T) {
	// Frames as the macOS sampler reports them for a CLI that spends most of
	// its wall time blocked: only the Go symbols can be patched.
	functions := []profile.Function{
		{Name: "__psynch_cvwait"},
		{Name: "github.com/itchyny/gojq/cli.newJSONInputIter.func1"},
		{Name: "_pthread_cond_wait"},
		{Name: "kevent"},
		{Name: "encoding/json/v2.Unmarshal"},
		{Name: "nanosleep"},
		{Name: "runtime.mallocgc"},
		{Name: "testing.(*B).runN"},
		{Name: "github.com/tomnomnom/gron.BenchmarkBigJSON"},
		{Name: "github.com/tomnomnom/gron.TestFill"},
	}
	got := hotFunctionNames(functions, 15)
	want := []string{
		"github.com/itchyny/gojq/cli.newJSONInputIter.func1",
		"encoding/json/v2.Unmarshal",
	}
	if !slices.Equal(got, want) {
		t.Errorf("hotFunctionNames() = %v, want %v", got, want)
	}
}

func TestFunctionNameCandidatesUnwrapsClosuresAndReceivers(t *testing.T) {
	tests := []struct {
		name string
		want string // candidate that must be present for repo search to match
	}{
		{"github.com/itchyny/gojq/cli.newJSONInputIter.func1", "newJSONInputIter"},
		{"pkg.outer.func1.2", "outer"},
		{"github.com/x/y.(*Encoder).encode", "encode"},
		{"main.main", "main"},
	}
	for _, tt := range tests {
		got := functionNameCandidates(tt.name)
		if !slices.Contains(got, tt.want) {
			t.Errorf("functionNameCandidates(%q) = %v, want it to contain %q", tt.name, got, tt.want)
		}
		if got[0] != tt.name {
			t.Errorf("functionNameCandidates(%q) first = %q, want the original name first", tt.name, got[0])
		}
	}
}

func TestRepoRelativeRewritesProfilerPaths(t *testing.T) {
	root := t.TempDir()
	engine := &Engine{state: State{Repository: root}}
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"absolute inside repo", filepath.Join(root, "statements.go"), "statements.go", true},
		{"absolute nested", filepath.Join(root, "cmd", "main.go"), "cmd/main.go", true},
		{"already relative", "identifier.go", "identifier.go", true},
		{"standard library", "/opt/homebrew/Cellar/go/1.27.0/libexec/src/unicode/graphic.go", "", false},
		{"repository root itself", root, "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := engine.repoRelative(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("repoRelative(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("repoRelative(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestAmplifyStdinKeepsJSONValid(t *testing.T) {
	seed := []byte(`{"meta":{"ok":true},"users":[{"id":0},{"id":1},{"id":2}],"trailing":"x"}`)
	amplified := amplifyStdin(seed)
	if len(amplified) <= len(seed) {
		t.Fatalf("amplified %d bytes, want more than the %d byte seed", len(amplified), len(seed))
	}
	var doc struct {
		Meta     map[string]bool      `json:"meta"`
		Users    []map[string]float64 `json:"users"`
		Trailing string               `json:"trailing"`
	}
	if err := json.Unmarshal(amplified, &doc); err != nil {
		t.Fatalf("amplified output is not valid JSON: %v", err)
	}
	// Growth must come from the array: repeating the whole document would have
	// made this invalid JSON, which is the bug the amplifier exists to avoid.
	if len(doc.Users) <= 3 || len(doc.Users)%3 != 0 {
		t.Fatalf("users = %d, want a multiple of 3 above 3", len(doc.Users))
	}
	if !doc.Meta["ok"] || doc.Trailing != "x" {
		t.Fatalf("document around the array changed: %+v", doc)
	}
	for i, user := range doc.Users {
		if id, ok := user["id"]; !ok || id != float64(i%3) {
			t.Fatalf("element %d = %v, want the replicated id %d", i, user, i%3)
		}
	}
}

func TestAmplifyStdinRepeatsNonJSONInput(t *testing.T) {
	seed := []byte("plain text line\n")
	amplified := amplifyStdin(seed)
	if len(amplified) <= len(seed) {
		t.Fatalf("amplified %d bytes, want more than %d", len(amplified), len(seed))
	}
	if string(amplified[:len(seed)]) != string(seed) {
		t.Fatalf("fallback must repeat the input verbatim, got %q", amplified[:len(seed)])
	}
	if len(amplified) < amplificationTarget {
		t.Fatalf("amplified only %d bytes, want at least %d", len(amplified), amplificationTarget)
	}
}

func TestAmplifyStdinLeavesUsableInputsAlone(t *testing.T) {
	oversized := make([]byte, maxAmplifiedStdin)
	if got := amplifyStdin(oversized); len(got) != len(oversized) {
		t.Fatalf("oversized input amplified to %d bytes, want %d", len(got), len(oversized))
	}
	if got := amplifyStdin(nil); len(got) != 0 {
		t.Fatalf("empty input amplified to %d bytes", len(got))
	}
	// An array with nothing in it gives the amplifier nothing to replicate, so
	// byte repetition takes over rather than emitting invalid JSON.
	for _, seed := range []string{`{"a":[]}`, `{"a":[  ]}`, "   "} {
		if got := amplifyStdin([]byte(seed)); len(got) <= len(seed) {
			t.Fatalf("input %q was not amplified: %d bytes", seed, len(got))
		}
	}
}

func TestLargestJSONArrayIgnoresBracketsInsideStrings(t *testing.T) {
	data := []byte(`{"note":"[not an array, just text]","keep":[[1,2],[3,4,5]]}`)
	start, end, ok := largestJSONArray(data)
	if !ok {
		t.Fatal("no array found")
	}
	if got := string(data[start+1 : end]); got != `[1,2],[3,4,5]` {
		t.Fatalf("largest array body = %q", got)
	}
	if _, _, ok := largestJSONArray([]byte(`{"note":"no arrays here"}`)); ok {
		t.Fatal("reported an array in a document that has none")
	}
}
