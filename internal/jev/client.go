// Package jev is the boundary to TypeSafe's Jev, reached through OpenRouter's
// System One API. Jev is not a text model: it answers typed questions about a
// piece of state with calibrated probabilities. gotorque asks it yes/no
// questions about measured hot functions, and deterministic code in this
// package turns the answers into a ranking. The answers are advice; nothing
// here decides whether a candidate is accepted.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Model is the exact Jev build every request is pinned to, and the only value
// Evaluate accepts as a matching answer (see Client.post). EnvAPIKey and
// EnvBaseURL name the only credential and endpoint inputs; the key is never
// written to campaign state.
const (
	Model          = "typesafe/jev-1.13-20260917"
	EnvAPIKey      = "OPENROUTER_API_KEY" //nolint:gosec // environment variable name, not a credential value
	EnvBaseURL     = "OPENROUTER_BASE_URL"
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	// EnvAllowDrift, set to any non-empty value, downgrades the canary drift
	// guard in Preflight from a failure to a warning. It has no effect on the
	// model or provider guard below: a wrong build or provider is refused
	// unconditionally, since nothing recorded is valid for any build or
	// provider other than the one it was measured against.
	EnvAllowDrift = "GOTORQUE_JEV_ALLOW_DRIFT"
	// pinnedProvider is the only provider gotorque accepts an answer from.
	// OpenRouter's System One API can in principle route a model id to more
	// than one upstream; TypeSafe is the only one that serves Jev, and every
	// baseline and canary in this package is only valid for its answers.
	pinnedProvider = "TypeSafe"
)

// Question is one typed question. gotorque sends only "noul" questions, whose
// criteria define what the true and false answers mean. OpenRouter's System
// One API also accepts "choice" and "score" but rejects "boolean" outright
// (HTTP 400), which is why this package's own boolean-shaped criteria are
// sent as "noul".
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Request asks every question against the same state in one round trip.
// Model is filled in by Evaluate when the caller leaves it empty; a caller
// never needs to set it.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one answer: the probability that the answer is yes. The wire
// shape carries the probability under "noul" ({"type":"noul","noul":0.91}),
// but answers are persisted verbatim in the bbolt jev_cache and in campaign
// events under the older field name "probability" (from an earlier endpoint
// this package used); UnmarshalJSON accepts either so neither live responses
// nor anything already on disk fail to decode.
type Answer struct {
	Type        string  `json:"type"`
	Probability float64 `json:"probability"`
}

func (a *Answer) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type        string   `json:"type"`
		Noul        *float64 `json:"noul"`
		Probability *float64 `json:"probability"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Type = raw.Type
	switch {
	case raw.Noul != nil:
		a.Probability = *raw.Noul
	case raw.Probability != nil:
		a.Probability = *raw.Probability
	default:
		return errors.New(`jev answer has neither "noul" nor "probability"`)
	}
	return nil
}

// Usage is System One's token and cost accounting for one request. Cost is in
// US dollars; System One prices Jev's output tokens free, so Cost tracks
// input-token spend only.
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// Response carries one answer per question id, plus which build and provider
// actually answered.
type Response struct {
	Model    string            `json:"model"`
	Answers  map[string]Answer `json:"answers"`
	Usage    Usage             `json:"usage"`
	ID       string            `json:"id,omitempty"`
	Provider string            `json:"provider,omitempty"`
}

// Evaluator answers typed questions. Client reaches System One; Stub answers
// without a network for --adk-stub runs.
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (Response, error)
}

// defaultBackoff is the wait before each retry of a throttled or failed call.
// The provider throttles hard: a benchmark at twelve parallel requests saw HTTP
// 429 ("upstream provider is currently experiencing high demand") on 36% of
// attempts, and even strictly sequential calls met a 429 that outlasted a 15 s
// ladder. Thirty-one seconds of backoff per call is still small against the
// analyst node's deadline, and a site that exhausts it is reported
// unclassified rather than failing the node.
var defaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// Client calls OpenRouter's System One evaluation endpoint.
type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	// Backoff overrides defaultBackoff; its length is the number of retries.
	Backoff []time.Duration
}

func NewClientFromEnvironment() Client {
	return Client{APIKey: os.Getenv(EnvAPIKey), BaseURL: os.Getenv(EnvBaseURL)}
}

func (c Client) endpoint() string {
	if base := strings.TrimRight(c.BaseURL, "/"); base != "" {
		return base + "/systemone"
	}
	return DefaultBaseURL + "/systemone"
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Jev answers in about half a second; a minute covers a slow endpoint
	// without letting one stuck call hold the analyst node.
	return &http.Client{Timeout: time.Minute}
}

func (c Client) backoff() []time.Duration {
	if c.Backoff != nil {
		return c.Backoff
	}
	return defaultBackoff
}

// Evaluate sends req, retrying throttled and transient failures.
func (c Client) Evaluate(ctx context.Context, req Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, errors.New(EnvAPIKey + " is required for --analyst jev")
	}
	if req.Model == "" {
		req.Model = Model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("encode Jev request: %w", err)
	}
	ladder := c.backoff()
	for attempt := 0; ; attempt++ {
		resp, retry, err := c.post(ctx, body)
		if err == nil {
			return resp, nil
		}
		if !retry || attempt >= len(ladder) {
			return Response{}, err
		}
		if err := sleep(ctx, ladder[attempt]); err != nil {
			return Response{}, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// post makes one attempt and reports whether a failure is worth retrying.
func (c Client) post(ctx context.Context, body []byte) (Response, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return Response{}, false, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return Response{}, ctx.Err() == nil, fmt.Errorf("request to Jev: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return Response{}, true, fmt.Errorf("read Jev response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return Response{}, retryable(httpResp.StatusCode), fmt.Errorf("OpenRouter returned HTTP %d for Jev: %s", httpResp.StatusCode, gatewayMessage(data))
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Response{}, false, fmt.Errorf("decode Jev response: %w", err)
	}
	if err := checkBuildAndProvider(resp); err != nil {
		return Response{}, false, err
	}
	return resp, false, nil
}

// checkBuildAndProvider refuses an answer that did not come from the exact
// pinned build (Model) served by the exact pinned provider (pinnedProvider).
// Both are positive checks, not a default-deny on absence: a response
// carrying no model or provider at all (a test stub, or a shape change) is
// accepted, since that is not evidence of a wrong build or provider, just of
// not knowing.
func checkBuildAndProvider(resp Response) error {
	if resp.Model != "" && resp.Model != Model {
		return fmt.Errorf("answered by build %q, not the pinned build %q; refusing an answer no baseline or canary was measured against", resp.Model, Model)
	}
	if resp.Provider != "" && resp.Provider != pinnedProvider {
		return fmt.Errorf("answered through provider %q, not %q; refusing an answer the baseline was never measured against", resp.Provider, pinnedProvider)
	}
	return nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// gatewayMessage extracts System One's own error text, e.g. "Free tier users
// do not have access to this model."
func gatewayMessage(data []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// Preflight spends one minimal request to prove the key, the account's
// access, and the pinned build all work before a campaign starts repository
// work. That one request carries the canary questions (canary.go) instead of
// a plain connectivity check, so the same call also catches Jev's answers
// moving even though the build id is pinned exactly (a release that changes
// answers without changing the id behind an alias TypeSafe still controls).
//
// The build and provider are checked on every response (checkBuildAndProvider,
// via Evaluate), so a wrong one already fails this call outright with no
// override; EnvAllowDrift only downgrades the canary answer-drift check.
func (c Client) Preflight(ctx context.Context) ([]string, error) {
	answers, err := c.askCanary(ctx)
	if err != nil {
		return nil, err
	}
	if len(canaryDrift(answers)) > 0 {
		if answers, err = c.confirmCanary(ctx, answers); err != nil {
			return nil, err
		}
	}
	var warnings []string
	if drift := canaryDrift(answers); len(drift) > 0 {
		msg := fmt.Sprintf("Jev's canary answers moved: %s; re-measure with TestLiveCanary", strings.Join(drift, "; "))
		if err := driftGuard(msg, true, &warnings); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// canaryConfirmations is how many more times Preflight asks the canary after
// a drifted first answer. Jev's answers are not only noisy at the 0.01 level
// the recorded sd shows: on the pinned build, one of ten repeats of the
// canary came back 0.17 against a recorded 0.248 (sd 0.014), and failing the
// preflight on that one sample stopped a campaign for nothing. The median of
// three drops a single outlier and still fails on a model that really moved.
const canaryConfirmations = 2

func (c Client) askCanary(ctx context.Context) (map[string]Answer, error) {
	resp, err := c.Evaluate(ctx, Request{State: canaryState, Questions: canaryQuestionSet()})
	if err != nil {
		return nil, fmt.Errorf("preflight against Jev: %w", err)
	}
	if len(resp.Answers) == 0 {
		return nil, errors.New("preflight against Jev: response carried no answer")
	}
	return resp.Answers, nil
}

// confirmCanary asks the canary canaryConfirmations more times and returns
// each question's median answer across first and the confirmations.
func (c Client) confirmCanary(ctx context.Context, first map[string]Answer) (map[string]Answer, error) {
	samples := []map[string]Answer{first}
	for range canaryConfirmations {
		answers, err := c.askCanary(ctx)
		if err != nil {
			return nil, err
		}
		samples = append(samples, answers)
	}
	return medianAnswers(samples), nil
}

// medianAnswers takes, for every question the first sample answered, the
// median probability across the samples that answered it.
func medianAnswers(samples []map[string]Answer) map[string]Answer {
	out := make(map[string]Answer, len(samples[0]))
	for id, answer := range samples[0] {
		ps := make([]float64, 0, len(samples))
		for _, s := range samples {
			if a, ok := s[id]; ok {
				ps = append(ps, a.Probability)
			}
		}
		slices.Sort(ps)
		out[id] = Answer{Type: answer.Type, Probability: ps[len(ps)/2]}
	}
	return out
}

// driftGuard applies EnvAllowDrift to one preflight finding: msg is appended
// as a warning either when hardFail is false or when the override is set;
// otherwise msg becomes the preflight's error.
func driftGuard(msg string, hardFail bool, warnings *[]string) error {
	if !hardFail || os.Getenv(EnvAllowDrift) != "" {
		*warnings = append(*warnings, msg)
		return nil
	}
	return fmt.Errorf("%s (set %s=1 to run anyway)", msg, EnvAllowDrift)
}

// Stub answers every question with probability one half. It never represents
// model judgment; it exists so --adk-stub can drive the cause-analyst node end
// to end without a network or a key.
type Stub struct{}

func (Stub) Evaluate(_ context.Context, req Request) (Response, error) {
	answers := make(map[string]Answer, len(req.Questions))
	for id, q := range req.Questions {
		answers[id] = Answer{Type: q.Type, Probability: 0.5}
	}
	return Response{Model: Model, Provider: pinnedProvider, Answers: answers}, nil
}
