package jev

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveBaseline measures the baseline in baseline.go and the fix-kind
// baseline in fixkinds.go against the real endpoint, in one request per
// function (cause and kind questions combined, as production sends them
// since ADR 0028 - see combinedQuestions in internal/campaign/causes.go). It
// is opt-in and spends one request per function:
//
//	OPENROUTER_API_KEY=... GOTORQUE_JEV_BASELINE_CASES=functions.jsonl \
//	  go test ./internal/jev -run TestLiveBaseline -v
//
// Each input line is {"id": "...", "path": "pkg/file.go", "source": "func ..."}.
// Set GOTORQUE_JEV_BASELINE_OUT to also write every answer as JSON lines.
func TestLiveBaseline(t *testing.T) {
	casesPath := os.Getenv("GOTORQUE_JEV_BASELINE_CASES")
	client := NewClientFromEnvironment()
	if casesPath == "" || client.APIKey == "" {
		t.Skip("set GOTORQUE_JEV_BASELINE_CASES and " + EnvAPIKey + " to measure the baseline")
	}
	// A baseline run is a few hundred calls back to back; give each one far
	// more patience than a campaign would, since a gap here costs a rerun.
	client.Backoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 60 * time.Second}
	functions := readBaselineFunctions(t, casesPath)
	out := baselineOutput(t)
	causeSamples := map[Cause][]float64{}
	kindSamples := map[FixKind][]float64{}
	questions := combinedBaselineQuestions()
	for _, fn := range functions {
		resp, err := client.Evaluate(context.Background(), Request{State: SiteState(fn.Path, fn.Source), Questions: questions})
		if err != nil {
			t.Fatalf("%s: %v", fn.ID, err)
		}
		for _, cause := range Causes {
			causeSamples[cause] = append(causeSamples[cause], resp.Answers[string(cause)].Probability)
		}
		for _, kind := range FixKinds {
			kindSamples[kind] = append(kindSamples[kind], resp.Answers[string(kind)].Probability)
		}
		if out != nil {
			line, _ := json.Marshal(map[string]any{"id": fn.ID, "answers": resp.Answers, "usage": resp.Usage})
			_, _ = fmt.Fprintln(out, string(line))
		}
	}
	t.Log("\n" + formatBaseline(causeSamples, len(functions)))
	t.Log("\n" + formatKindBaseline(kindSamples, len(functions)))
}

// combinedBaselineQuestions merges the cause and fix-kind question sets, the
// same shape production sends in one request per site (ADR 0028), so the
// measured baseline matches what a live campaign actually asks.
func combinedBaselineQuestions() map[string]Question {
	questions := Questions()
	for id, q := range KindQuestions() {
		questions[id] = q
	}
	return questions
}

func formatKindBaseline(samples map[FixKind][]float64, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// measured over %d functions\nvar kindBaseline = map[FixKind]stats{\n", n)
	for _, kind := range FixKinds {
		mean, std := meanStd(samples[kind])
		fmt.Fprintf(&b, "\t%s: {mean: %.4f, std: %.4f},\n", kindIdent(kind), mean, std)
	}
	fmt.Fprintf(&b, "}\n\nconst kindBaselineDigest = %q\n", kindDigest())
	return b.String()
}

func kindIdent(k FixKind) string {
	names := map[FixKind]string{
		KindBuilder: "KindBuilder", KindDropFmt: "KindDropFmt", KindASCII: "KindASCII",
		KindCommonCase: "KindCommonCase", KindComputeOnce: "KindComputeOnce", KindRemoveUnused: "KindRemoveUnused",
		KindConversion: "KindConversion", KindStackScratch: "KindStackScratch", KindSizeHint: "KindSizeHint",
	}
	return names[k]
}

type baselineFunction struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

func readBaselineFunctions(t *testing.T, path string) []baselineFunction {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var functions []baselineFunction
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var fn baselineFunction
		if err := json.Unmarshal(scanner.Bytes(), &fn); err != nil {
			t.Fatal(err)
		}
		functions = append(functions, fn)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return functions
}

func baselineOutput(t *testing.T) *os.File {
	t.Helper()
	path := os.Getenv("GOTORQUE_JEV_BASELINE_OUT")
	if path == "" {
		return nil
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func formatBaseline(samples map[Cause][]float64, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// measured over %d functions\nvar baseline = map[Cause]stats{\n", n)
	for _, cause := range Causes {
		mean, std := meanStd(samples[cause])
		fmt.Fprintf(&b, "\t%s: {mean: %.4f, std: %.4f},\n", causeIdent(cause), mean, std)
	}
	fmt.Fprintf(&b, "}\n\nconst baselineDigest = %q\n", digest())
	return b.String()
}

func meanStd(xs []float64) (float64, float64) {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(sq / float64(len(xs)))
}

func causeIdent(c Cause) string {
	names := map[Cause]string{CauseAlloc: "CauseAlloc", CauseUnbufferedIO: "CauseUnbufferedIO", CauseStringBuild: "CauseStringBuild", CauseFastPath: "CauseFastPath", CauseSuperlinear: "CauseSuperlinear", CausePrealloc: "CausePrealloc", CauseRedundant: "CauseRedundant"}
	return names[c]
}

// TestLiveReviewBaseline measures the review baseline in review_baseline.go:
//
//	OPENROUTER_API_KEY=... GOTORQUE_JEV_REVIEW_CASES=patches.jsonl \
//	  go test ./internal/jev -run TestLiveReviewBaseline -v
//
// Each input line is {"id", "hypothesis", "function", "source", "patch"}: a real
// performance patch and its function's source before it. Set
// GOTORQUE_JEV_REVIEW_OUT to also write every answer as JSON lines.
func TestLiveReviewBaseline(t *testing.T) {
	casesPath := os.Getenv("GOTORQUE_JEV_REVIEW_CASES")
	client := NewClientFromEnvironment()
	if casesPath == "" || client.APIKey == "" {
		t.Skip("set GOTORQUE_JEV_REVIEW_CASES and " + EnvAPIKey + " to measure the review baseline")
	}
	client.Backoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 60 * time.Second}
	patches := readReviewPatches(t, casesPath)
	out := reviewOutput(t)
	samples := map[Hazard][]float64{}
	for _, p := range patches {
		resp, err := client.Evaluate(context.Background(), Request{State: ReviewState(p.Hypothesis, p.Function, p.Source, p.Patch), Questions: ReviewQuestions()})
		if err != nil {
			t.Fatalf("%s: %v", p.ID, err)
		}
		for _, h := range Hazards {
			samples[h] = append(samples[h], resp.Answers[string(h)].Probability)
		}
		if out != nil {
			line, _ := json.Marshal(map[string]any{"id": p.ID, "answers": resp.Answers, "usage": resp.Usage})
			_, _ = fmt.Fprintln(out, string(line))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// measured over %d patches\nvar reviewBaseline = map[Hazard]stats{\n", len(patches))
	for _, h := range Hazards {
		mean, std := meanStd(samples[h])
		fmt.Fprintf(&b, "\t%q: {mean: %.4f, std: %.4f},\n", h, mean, std)
	}
	fmt.Fprintf(&b, "}\n\nconst reviewBaselineDigest = %q\n", reviewDigest())
	t.Log("\n" + b.String())
}

func reviewOutput(t *testing.T) *os.File {
	t.Helper()
	path := os.Getenv("GOTORQUE_JEV_REVIEW_OUT")
	if path == "" {
		return nil
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

type reviewPatch struct {
	ID         string `json:"id"`
	Hypothesis string `json:"hypothesis"`
	Function   string `json:"function"`
	Source     string `json:"source"`
	Patch      string `json:"patch"`
}

func readReviewPatches(t *testing.T, path string) []reviewPatch {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	patches := make([]reviewPatch, 0, len(lines))
	for _, line := range lines {
		var p reviewPatch
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatal(err)
		}
		patches = append(patches, p)
	}
	return patches
}
