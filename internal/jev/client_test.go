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

// gateway serves the given status and body for each successive call, repeating
// the last pair once the script runs out.
func gateway(t *testing.T, script ...func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
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

const answered = `{"model":"typesafe-ai/jev","answers":{"ok":{"type":"boolean","probability":0.93}},"usage":{"inputTokens":120,"outputTokens":9}}`

func TestEvaluateSendsTheRequestTheGatewayExpects(t *testing.T) {
	var got Request
	srv, _ := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/evaluate" {
			t.Errorf("path = %q, want /v1/evaluate", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key-1" {
			t.Errorf("authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(answered))
	})
	client := Client{APIKey: "key-1", BaseURL: srv.URL + "/v1/"}
	resp, err := client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"ok": {Type: "boolean", Instructions: "ok?"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != Model {
		t.Errorf("model = %q, want the default %q", got.Model, Model)
	}
	if resp.Answers["ok"].Probability != 0.93 || resp.Usage.InputTokens != 120 {
		t.Errorf("response = %+v", resp)
	}
}

func TestEvaluatePinsTheProviderByDefault(t *testing.T) {
	var got Request
	srv, _ := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(answered))
	})
	client := Client{APIKey: "k", BaseURL: srv.URL}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if got.ProviderOptions == nil || len(got.ProviderOptions.Gateway.Only) != 1 || got.ProviderOptions.Gateway.Only[0] != pinnedProvider {
		t.Errorf("providerOptions = %+v, want only [%s]", got.ProviderOptions, pinnedProvider)
	}
}

func TestEvaluateKeepsACallerSProviderOptions(t *testing.T) {
	var got Request
	srv, _ := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(answered))
	})
	client := Client{APIKey: "k", BaseURL: srv.URL}
	custom := &ProviderOptions{Gateway: GatewayOptions{Only: []string{"something-else"}}}
	if _, err := client.Evaluate(context.Background(), Request{ProviderOptions: custom}); err != nil {
		t.Fatal(err)
	}
	if got.ProviderOptions.Gateway.Only[0] != "something-else" {
		t.Errorf("providerOptions = %+v, want the caller's own", got.ProviderOptions)
	}
}

func TestEvaluateRefusesAWrongFinalProvider(t *testing.T) {
	body := `{"model":"typesafe-ai/jev","answers":{"ok":{"type":"boolean","probability":0.5}},"providerMetadata":{"gateway":{"routing":{"finalProvider":"digitalocean"}}}}`
	srv, calls := gateway(t, respond(http.StatusOK, body))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "digitalocean") {
		t.Fatalf("err = %v, want it to name the wrong provider", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a wrong provider", calls.Load())
	}
}

func TestEvaluateAcceptsThePinnedProvider(t *testing.T) {
	body := `{"model":"typesafe-ai/jev","answers":{"ok":{"type":"boolean","probability":0.5}},"providerMetadata":{"gateway":{"routing":{"finalProvider":"typesafe-ai"}}}}`
	srv, _ := gateway(t, respond(http.StatusOK, body))
	client := Client{APIKey: "k", BaseURL: srv.URL}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatalf("err = %v, want the pinned provider accepted", err)
	}
}

func TestEvaluateAcceptsNoRoutingMetadataAtAll(t *testing.T) {
	// Stub gateways in tests, and possibly a real response the gateway
	// changes shape on, may carry no providerMetadata at all; that must not
	// be treated as a wrong provider.
	if _, err := (Client{APIKey: "k", BaseURL: mustAnswerServer(t)}).Evaluate(context.Background(), Request{}); err != nil {
		t.Fatalf("err = %v, want a response with no routing metadata accepted", err)
	}
}

func mustAnswerServer(t *testing.T) string {
	t.Helper()
	srv, _ := gateway(t, respond(http.StatusOK, answered))
	return srv.URL
}

func TestEvaluateRetriesThrottledCalls(t *testing.T) {
	srv, calls := gateway(t, respond(http.StatusTooManyRequests, `{"error":{"message":"busy"}}`), respond(http.StatusBadGateway, "down"), respond(http.StatusOK, answered))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0, 0, 0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestEvaluateGivesUpAfterTheLadder(t *testing.T) {
	srv, calls := gateway(t, respond(http.StatusTooManyRequests, `{"error":{"message":"The upstream provider is currently experiencing high demand."}}`))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "high demand") {
		t.Fatalf("err = %v, want the gateway's 429 message", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want one try plus one retry", calls.Load())
	}
}

func TestEvaluateDoesNotRetryARefusal(t *testing.T) {
	srv, calls := gateway(t, respond(http.StatusForbidden, `{"error":{"message":"AI Gateway requires a valid credit card on file to service requests."}}`))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0, 0}}
	_, err := client.Evaluate(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "credit card") {
		t.Fatalf("err = %v, want the gateway's own explanation", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a 403", calls.Load())
	}
}

func TestEvaluateRejectsAMalformedAnswer(t *testing.T) {
	srv, calls := gateway(t, respond(http.StatusOK, "not json"))
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "decode Jev response") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry for a malformed body", calls.Load())
	}
}

func TestEvaluateRetriesAnUnreachableGateway(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	client := Client{APIKey: "k", BaseURL: srv.URL, Backoff: []time.Duration{0}}
	if _, err := client.Evaluate(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "request to Jev") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvaluateStopsWaitingWhenTheContextEnds(t *testing.T) {
	srv, _ := gateway(t, respond(http.StatusTooManyRequests, "busy"))
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

// canaryAnsweredBody builds an /v1/evaluate response that answers every
// canary question at its recorded value, so a mock gateway can drive
// Preflight without tripping canary drift by accident. overrides replaces
// individual answers, e.g. to test drift.
func canaryAnsweredBody(overrides map[string]float64) string {
	answers := map[string]Answer{}
	for id, p := range canaryRecorded {
		answers[id] = Answer{Type: "boolean", Probability: p}
	}
	for id, p := range overrides {
		answers[id] = Answer{Type: "boolean", Probability: p}
	}
	body, err := json.Marshal(map[string]any{"model": Model, "answers": answers})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// preflightGateway serves canaryAnsweredBody(overrides) for /v1/evaluate and
// a models listing naming release for the "jev" entry, from one server, so
// Client.BaseURL can point a Preflight call at both endpoints it uses.
func preflightGateway(t *testing.T, release string, overrides map[string]float64) *httptest.Server {
	t.Helper()
	evalBody := canaryAnsweredBody(overrides)
	modelsBody, err := json.Marshal(map[string]any{"models": []map[string]string{{"name": "jev", "release_date": release}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/typesafe/v1/models") {
			_, _ = w.Write(modelsBody)
			return
		}
		_, _ = w.Write([]byte(evalBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPreflightSucceedsWithNoDrift(t *testing.T) {
	srv := preflightGateway(t, baselineModelRelease, nil)
	warnings, err := (Client{APIKey: "k", BaseURL: srv.URL}).Preflight(context.Background())
	if err != nil || len(warnings) != 0 {
		t.Errorf("preflight = %v, %v, want no warnings and no error", warnings, err)
	}
}

func TestPreflightRejectsAResponseWithNoAnswer(t *testing.T) {
	empty, _ := gateway(t, respond(http.StatusOK, `{"answers":{}}`))
	if _, err := (Client{APIKey: "k", BaseURL: empty.URL}).Preflight(context.Background()); err == nil {
		t.Error("preflight accepted a response with no answer")
	}
}

func TestPreflightPropagatesARefusal(t *testing.T) {
	refused, _ := gateway(t, respond(http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`))
	if _, err := (Client{APIKey: "k", BaseURL: refused.URL}).Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("preflight = %v, want the refusal", err)
	}
}

func TestPreflightFailsOnAReleaseDateMismatch(t *testing.T) {
	srv := preflightGateway(t, "2099-01-01", nil)
	client := Client{APIKey: "k", BaseURL: srv.URL}
	_, err := client.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TestLiveBaseline") || !strings.Contains(err.Error(), EnvAllowDrift) {
		t.Fatalf("err = %v, want it to name TestLiveBaseline and %s", err, EnvAllowDrift)
	}
}

func TestPreflightWarnsOnAReleaseDateMismatchUnderTheOverride(t *testing.T) {
	t.Setenv(EnvAllowDrift, "1")
	srv := preflightGateway(t, "2099-01-01", nil)
	client := Client{APIKey: "k", BaseURL: srv.URL}
	warnings, err := client.Preflight(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want the override to downgrade it", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "TestLiveBaseline") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestPreflightWarnsWhenTheModelsListingCannotBeRead(t *testing.T) {
	// This server answers /v1/evaluate correctly but has no real
	// /typesafe/v1/models handler, so the listing decodes to no "jev" entry.
	// That must warn, never fail the preflight: not knowing the release date
	// is not evidence the model drifted.
	srv, _ := gateway(t, respond(http.StatusOK, canaryAnsweredBody(nil)))
	client := Client{APIKey: "k", BaseURL: srv.URL}
	warnings, err := client.Preflight(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want an unreadable listing to only warn", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

func TestPreflightFailsOnCanaryDrift(t *testing.T) {
	moved := string(canaryCauses[0])
	srv := preflightGateway(t, baselineModelRelease, map[string]float64{moved: canaryRecorded[moved] + 0.3})
	client := Client{APIKey: "k", BaseURL: srv.URL}
	_, err := client.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TestLiveCanary") || !strings.Contains(err.Error(), moved) {
		t.Fatalf("err = %v, want it to name TestLiveCanary and %s", err, moved)
	}
}

func TestPreflightWarnsOnCanaryDriftUnderTheOverride(t *testing.T) {
	t.Setenv(EnvAllowDrift, "1")
	moved := string(canaryCauses[0])
	srv := preflightGateway(t, baselineModelRelease, map[string]float64{moved: canaryRecorded[moved] + 0.3})
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
	if c.APIKey != "from-env" || c.endpoint() != DefaultBaseURL+"/evaluate" {
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
