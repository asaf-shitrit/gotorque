package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestLiveCanary measures canaryRecorded against the real endpoint, repeating
// the same fixed request and averaging. It is opt-in and spends a handful of
// requests:
//
//	OPENROUTER_API_KEY=... go test ./internal/jev -run TestLiveCanary -v
//
// Set GOTORQUE_JEV_CANARY_REPEATS to change the repeat count (default 4).
// Set GOTORQUE_JEV_CANARY_OUT to also write every repeat's answers as JSON
// lines.
func TestLiveCanary(t *testing.T) {
	client := NewClientFromEnvironment()
	if client.APIKey == "" {
		t.Skip("set " + EnvAPIKey + " to measure the canary")
	}
	repeats := canaryRepeats(t)
	out := canaryOutput(t)
	questions := canaryQuestionSet()
	samples := map[string][]float64{}
	for i := 0; i < repeats; i++ {
		samples = canaryRepeat(t, client, questions, i, samples, out)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// measured over %d repeats\nvar canaryRecorded = map[string]float64{\n", repeats)
	for _, cause := range canaryCauses {
		mean, std := meanStd(samples[string(cause)])
		fmt.Fprintf(&b, "\t%q: %.4f, // sd %.4f\n", string(cause), mean, std)
	}
	fmt.Fprintf(&b, "}\n\nconst canaryDigest = %q\n", checkCanaryDigest())
	t.Log("\n" + b.String())
}

// canaryRepeats reads GOTORQUE_JEV_CANARY_REPEATS, defaulting to 4.
func canaryRepeats(t *testing.T) int {
	t.Helper()
	repeats := 4
	if v := os.Getenv("GOTORQUE_JEV_CANARY_REPEATS"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &repeats); err != nil || n != 1 {
			t.Fatalf("GOTORQUE_JEV_CANARY_REPEATS = %q: %v", v, err)
		}
	}
	return repeats
}

// canaryOutput opens GOTORQUE_JEV_CANARY_OUT for appending every repeat's raw
// answers as JSON lines, or returns nil when unset.
func canaryOutput(t *testing.T) *os.File {
	t.Helper()
	path := os.Getenv("GOTORQUE_JEV_CANARY_OUT")
	if path == "" {
		return nil
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	return out
}

// canaryRepeat sends one canary request, records its answers into samples,
// and appends the raw response to out when it is not nil.
func canaryRepeat(t *testing.T, client Client, questions map[string]Question, i int, samples map[string][]float64, out *os.File) map[string][]float64 {
	t.Helper()
	resp, err := client.Evaluate(context.Background(), Request{State: canaryState, Questions: questions})
	if err != nil {
		t.Fatalf("attempt %d: %v", i, err)
	}
	for _, cause := range canaryCauses {
		id := string(cause)
		answer, ok := resp.Answers[id]
		if !ok {
			t.Fatalf("attempt %d: no answer to %s", i, id)
		}
		samples[id] = append(samples[id], answer.Probability)
	}
	if out != nil {
		line, _ := json.Marshal(map[string]any{"repeat": i, "answers": resp.Answers, "usage": resp.Usage})
		_, _ = fmt.Fprintln(out, string(line))
	}
	return samples
}
