package jev

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestLiveCanary measures canaryRecorded against the real gateway, repeating
// the same fixed request and averaging. It is opt-in and spends a handful of
// requests:
//
//	AI_GATEWAY_API_KEY=... go test ./internal/jev -run TestLiveCanary -v
//
// Set GOTORQUE_JEV_CANARY_REPEATS to change the repeat count (default 4).
func TestLiveCanary(t *testing.T) {
	client := NewClientFromEnvironment()
	if client.APIKey == "" {
		t.Skip("set " + EnvAPIKey + " to measure the canary")
	}
	repeats := 4
	if v := os.Getenv("GOTORQUE_JEV_CANARY_REPEATS"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &repeats); err != nil || n != 1 {
			t.Fatalf("GOTORQUE_JEV_CANARY_REPEATS = %q: %v", v, err)
		}
	}
	questions := canaryQuestionSet()
	samples := map[string][]float64{}
	for i := 0; i < repeats; i++ {
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
