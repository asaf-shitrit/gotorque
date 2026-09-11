package campaign

import (
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
