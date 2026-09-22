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

func TestPreflight(t *testing.T) {
	ok, _ := gateway(t, respond(http.StatusOK, answered))
	if err := (Client{APIKey: "k", BaseURL: ok.URL}).Preflight(context.Background()); err != nil {
		t.Errorf("preflight = %v", err)
	}
	empty, _ := gateway(t, respond(http.StatusOK, `{"answers":{}}`))
	if err := (Client{APIKey: "k", BaseURL: empty.URL}).Preflight(context.Background()); err == nil {
		t.Error("preflight accepted a response with no answer")
	}
	refused, _ := gateway(t, respond(http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`))
	if err := (Client{APIKey: "k", BaseURL: refused.URL}).Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("preflight = %v, want the refusal", err)
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
