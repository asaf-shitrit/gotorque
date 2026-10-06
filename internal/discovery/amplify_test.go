package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/profile"
)

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

// TestAmplifyRepeatsScalesOnlyDeclaredRepeatableInputs: a fixture or stdin the
// manifest wrote with a repeat count grows to about amplificationTarget for
// the sampled run; an input without one (a script) is left alone, and a count
// already larger is kept.
func TestAmplifyRepeatsScalesOnlyDeclaredRepeatableInputs(t *testing.T) {
	block := strings.Repeat("x", 100)
	seed := manifest.SeedWorkload{
		ID:    "s",
		Stdin: block, StdinHeader: "h\n", StdinRepeat: 3,
		Files: []manifest.FixtureFile{
			{Path: "rows.csv", Header: "id\n", Content: block, Repeat: 10},
			{Path: "bench.lua", Content: "print(1)\n"},
			{Path: "huge.txt", Content: block, Repeat: 1 << 20},
		},
	}
	got := amplifyRepeats(seed, amplificationTarget)
	want := (amplificationTarget - 3) / 100
	require.Equal(t, want, got.Files[0].Repeat)
	require.Equal(t, 0, got.Files[1].Repeat, "a script has no repeat and is not repeated")
	require.Equal(t, 1<<20, got.Files[2].Repeat, "a count already past the target is kept")
	require.Equal(t, (amplificationTarget-2)/100, got.StdinRepeat)
	require.Equal(t, 10, seed.Files[0].Repeat, "the manifest's seed is not modified")

	plain := amplifyRepeats(manifest.SeedWorkload{Stdin: "{}"}, amplificationTarget)
	require.Equal(t, 0, plain.StdinRepeat)
	require.Equal(t, 5, scaledRepeat(0, 0, 5, amplificationTarget), "an empty block cannot be scaled")
}

// TestSamplingRetryAppliesOnlyToRepeatableInputs: the larger retry only makes
// sense for inputs the manifest declares repeatable; a script cannot grow.
func TestSamplingRetryAppliesOnlyToRepeatableInputs(t *testing.T) {
	require.True(t, hasRepeatableInput(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "a,b\n", Repeat: 10}}}))
	require.True(t, hasRepeatableInput(manifest.SeedWorkload{Stdin: "x\n", StdinRepeat: 2}))
	require.False(t, hasRepeatableInput(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "print(1)\n"}}}))

	big := amplifyRepeats(manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: strings.Repeat("x", 100), Repeat: 10}}}, retryAmplificationTarget)
	require.Equal(t, retryAmplificationTarget/100, big.Files[0].Repeat)
}

// TestRetriesLargerOnATargetThatEndedTooSoon: a target dead before the
// liveness check, or alive for it and gone before any sample landed (held-out
// csvq's empty call graph), retries larger when its inputs can grow.
func TestRetriesLargerOnATargetThatEndedTooSoon(t *testing.T) {
	repeatable := manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "a,b\n", Repeat: 10}}}
	script := manifest.SeedWorkload{Files: []manifest.FixtureFile{{Content: "print(1)\n"}}}
	require.True(t, retriesLarger(profile.ErrTargetExitedEarly, repeatable))
	require.True(t, retriesLarger(fmt.Errorf("sample: %w", profile.ErrNoFrames), repeatable))
	require.False(t, retriesLarger(profile.ErrNoFrames, script), "a script cannot grow")
	require.False(t, retriesLarger(errors.New("sample tool unavailable"), repeatable))
	require.False(t, retriesLarger(nil, repeatable))
}

// TestLineRepetitionFeedsALineOrientedMode: gron --stream reads one document per
// line, so the fallback input is the seed, newline-terminated, many times over.
func TestLineRepetitionFeedsALineOrientedMode(t *testing.T) {
	seed := []byte(`{"users":[1,2]}` + "\n")
	amplified := repeatLines(seed[:len(seed)-1])
	require.GreaterOrEqual(t, len(amplified), amplificationTarget)
	lines := strings.Split(strings.TrimSuffix(string(amplified), "\n"), "\n")
	require.Greater(t, len(lines), 1)
	for _, line := range lines {
		require.JSONEq(t, `{"users":[1,2]}`, line)
	}
	require.Equal(t, amplified, repeatLines(seed), "a trailing newline is not doubled")
}
