package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// systemOne serves the given status and body for each successive call,
// repeating the last pair once the script runs out.
func systemOne(t *testing.T, script ...func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		script[min(n, len(script)-1)](w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func respond(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const answered = `{"model":"typesafe/jev-1.13-20260917","provider":"TypeSafe","answers":{"ok":{"type":"noul","noul":0.93}},"usage":{"input_tokens":120,"output_tokens":9,"cost":0.00001}}`

func TestEvaluateSendsTheRequestSystemOneExpects(t *testing.T) {
	var got Request
	srv, _ := systemOne(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			t.Errorf("path = %q, want /systemone", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key-1" {
			t.Errorf("authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(answered))
	})
	client := Client{APIKey: "key-1", BaseURL: srv.URL}
	resp, err := client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"ok": {Type: "noul", Instructions: "ok?"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != Model {
		t.Errorf("model = %q, want the pinned build %q", got.Model, Model)
	}
	if resp.Answers["ok"].Probability != 0.93 || resp.Usage.InputTokens != 120 || resp.Usage.Cost != 0.00001 {
		t.Errorf("response = %+v", resp)
	}
}

func TestEvaluateTrimsATrailingSlashFromBaseURL(t *testing.T) {
	srv, _ := systemOne(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			t.Errorf("path = %q, want /systemone", r.URL.Path)
		}
		_, _ = w.Write([]byte(answered))
	})
	client := Client{APIKey: "k", BaseURL: srv.URL + "/"}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateRefusesAWrongBuild(t *testing.T) {
	body := `{"model":"typesafe/jev-1.14-20261001","provider":"TypeSafe","answers":{"ok":{"type":"noul","noul":0.5}}}`
	srv, calls := systemOne(t, respond(http.StatusOK, body))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "typesafe/jev-1.14-20261001") {
		t.Fatalf("err = %v, want it to name the wrong build", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a wrong build", calls.Load())
	}
}

func TestEvaluateRefusesAWrongProvider(t *testing.T) {
	body := `{"model":"typesafe/jev-1.13-20260917","provider":"SomeoneElse","answers":{"ok":{"type":"noul","noul":0.5}}}`
	srv, calls := systemOne(t, respond(http.StatusOK, body))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "SomeoneElse") {
		t.Fatalf("err = %v, want it to name the wrong provider", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a wrong provider", calls.Load())
	}
}

func TestEvaluateAcceptsThePinnedBuildAndProvider(t *testing.T) {
	srv, _ := systemOne(t, respond(http.StatusOK, answered))
	client := Client{APIKey: "k", BaseURL: srv.URL}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatalf("err = %v, want the pinned build and provider accepted", err)
	}
}

func TestEvaluateAcceptsNoBuildOrProviderAtAll(t *testing.T) {
	// A stub server in a test, or a real response System One changes shape
	// on, may carry no model or provider at all; that must not be treated as
	// a wrong build or provider.
	body := `{"answers":{"ok":{"type":"noul","noul":0.5}}}`
	srv, _ := systemOne(t, respond(http.StatusOK, body))
	if _, err := (Client{APIKey: "k", BaseURL: srv.URL}).Evaluate(context.Background(), Request{}); err != nil {
		t.Fatalf("err = %v, want a response with no build or provider accepted", err)
	}
}

func TestEvaluateRetriesThrottledCalls(t *testing.T) {
	srv, calls := systemOne(t, respond(http.StatusTooManyRequests, `{"error":{"message":"busy"}}`), respond(http.StatusBadGateway, "down"), respond(http.StatusOK, answered))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0, 0, 0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestEvaluateGivesUpAfterTheLadder(t *testing.T) {
	srv, calls := systemOne(t, respond(http.StatusTooManyRequests, `{"error":{"message":"The upstream provider is currently experiencing high demand."}}`))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "high demand") {
		t.Fatalf("err = %v, want the endpoint's 429 message", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want one try plus one retry", calls.Load())
	}
}

func TestEvaluateDoesNotRetryARefusal(t *testing.T) {
	srv, calls := systemOne(t, respond(http.StatusForbidden, `{"error":{"message":"Free tier users do not have access to this model."}}`))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0, 0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "Free tier") {
		t.Fatalf("err = %v, want the endpoint's own explanation", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a 403", calls.Load())
	}
}

func TestEvaluateRejectsAMalformedAnswer(t *testing.T) {
	srv, calls := systemOne(t, respond(http.StatusOK, "not json"))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "decode Jev response") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a malformed body", calls.Load())
	}
}

func TestEvaluateRejectsAnAnswerWithNeitherNoulNorProbability(t *testing.T) {
	srv, _ := systemOne(t, respond(http.StatusOK, `{"answers":{"ok":{"type":"noul"}}}`))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "decode Jev response") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvaluateDecodesAnAnswerUnderTheOlderProbabilityField(t *testing.T) {
	// Answers persisted in the bbolt jev_cache and campaign events were
	// written under "probability" from an earlier endpoint this package
	// used; a cache hit replayed through the same Answer type must still
	// decode.
	var a Answer
	if err := json.Unmarshal([]byte(`{"type":"noul","probability":0.42}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Probability != 0.42 {
		t.Errorf("probability = %v, want 0.42", a.Probability)
	}
}

func TestEvaluateRetriesAnUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "request to Jev") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvaluateStopsWaitingWhenTheContextEnds(t *testing.T) {
	srv, _ := systemOne(t, respond(http.StatusTooManyRequests, "busy"))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{time.Hour}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Evaluate(ctx, Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("backoff ignored the context")
	}
}

func TestEvaluateRequiresAKey(t *testing.T) {
	if _, err := (Client{}).Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), EnvAPIKey) {
		t.Fatalf("err = %v, want it to name %s", err, EnvAPIKey)
	}
}

func TestEvaluateReportsAnUnencodableState(t *testing.T) {
	_, err := Client{APIKey: "k"}.Evaluate(context.Background(), Request{State: make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "encode Jev request") {
		t.Fatalf("err = %v", err)
	}
}

// canaryAnsweredBody builds a /systemone response that answers every canary
// question at its recorded value, so a mock endpoint can drive Preflight
// without tripping canary drift by accident. overrides replaces individual
// answers, e.g. to test drift.
func canaryAnsweredBody(overrides map[string]float64) string {
	answers := map[string]map[string]any{}
	for id, p := range canaryRecorded {
		answers[id] = map[string]any{"type": "noul", "noul": p}
	}
	for id, p := range overrides {
		answers[id] = map[string]any{"type": "noul", "noul": p}
	}
	body, err := json.Marshal(map[string]any{"model": Model, "provider": pinnedProvider, "answers": answers})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestPreflightSucceedsWithNoDrift(t *testing.T) {
	srv, _ := systemOne(t, respond(http.StatusOK, canaryAnsweredBody(nil)))
	warnings, err := (Client{APIKey: "k", BaseURL: srv.URL}).Preflight(context.Background())
	if err != nil || len(warnings) != 0 {
		t.Errorf("preflight = %v, %v, want no warnings and no error", warnings, err)
	}
}

func TestPreflightRejectsAResponseWithNoAnswer(t *testing.T) {
	empty, _ := systemOne(t, respond(http.StatusOK, `{"answers":{}}`))
	if _, err := (Client{APIKey: "k", BaseURL: empty.URL}).Preflight(context.Background()); err == nil {
		t.Error("preflight accepted a response with no answer")
	}
}

func TestPreflightPropagatesARefusal(t *testing.T) {
	refused, _ := systemOne(t, respond(http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`))
	if _, err := (Client{APIKey: "k", BaseURL: refused.URL}).Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("preflight = %v, want the refusal", err)
	}
}

func TestPreflightFailsOnCanaryDrift(t *testing.T) {
	moved := string(canaryCauses[0])
	srv, _ := systemOne(t, respond(http.StatusOK, canaryAnsweredBody(map[string]float64{moved: canaryRecorded[moved] + 0.3})))
	client := Client{APIKey: "k", BaseURL: srv.URL}
	_, err := client.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TestLiveCanary") || !strings.Contains(err.Error(), moved) {
		t.Fatalf("err = %v, want it to name TestLiveCanary and %s", err, moved)
	}
}

func TestPreflightWarnsOnCanaryDriftUnderTheOverride(t *testing.T) {
	t.Setenv(EnvAllowDrift, "1")
	moved := string(canaryCauses[0])
	srv, _ := systemOne(t, respond(http.StatusOK, canaryAnsweredBody(map[string]float64{moved: canaryRecorded[moved] + 0.3})))
	client := Client{APIKey: "k", BaseURL: srv.URL}
	warnings, err := client.Preflight(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want the override to downgrade it", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "TestLiveCanary") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestGatewayMessageFallsBackToTheTruncatedBody(t *testing.T) {
	long := strings.Repeat("x", 400)
	if got := gatewayMessage([]byte(long)); len(got) > 310 || !strings.HasSuffix(got, "…") {
		t.Errorf("message = %q", got)
	}
	if got := gatewayMessage([]byte(" plain ")); got != "plain" {
		t.Errorf("message = %q", got)
	}
}

func TestClientDefaults(t *testing.T) {
	t.Setenv(EnvAPIKey, "from-env")
	t.Setenv(EnvBaseURL, "")
	c := NewClientFromEnvironment()
	if c.APIKey != "from-env" || c.endpoint() != DefaultBaseURL+"/systemone" {
		t.Errorf("client = %+v, endpoint %q", c, c.endpoint())
	}
	if c.httpClient().Timeout != time.Minute || len(c.backoff()) != len(defaultBackoff) {
		t.Error("defaults not applied")
	}
	custom := &http.Client{}
	if (Client{HTTP: custom}).httpClient() != custom {
		t.Error("custom HTTP client ignored")
	}
}

func TestStubAnswersEveryQuestion(t *testing.T) {
	resp, err := Stub{}.Evaluate(context.Background(), Request{Questions: Questions()})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Answers) != len(Causes) {
		t.Fatalf("answers = %d, want %d", len(resp.Answers), len(Causes))
	}
	if _, err := Rank(resp.Answers); err != nil {
		t.Errorf("stub answers do not rank: %v", err)
	}
}
