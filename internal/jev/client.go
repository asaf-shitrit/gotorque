// Package jev is the boundary to TypeSafe's Jev, reached through the Vercel AI
// Gateway. Jev is not a text model: it answers typed questions about a piece of
// state with calibrated probabilities. gotorque asks it yes/no questions about
// measured hot functions, and deterministic code in this package turns the
// answers into a ranking. The answers are advice; nothing here decides whether
// a candidate is accepted.
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
	"strings"
	"time"
)

// Model is the gateway slug for Jev. EnvAPIKey and EnvBaseURL name the only
// credential and endpoint inputs; the key is never written to campaign state.
const (
	Model          = "typesafe-ai/jev"
	EnvAPIKey      = "AI_GATEWAY_API_KEY" //nolint:gosec // environment variable name, not a credential value
	EnvBaseURL     = "AI_GATEWAY_BASE_URL"
	DefaultBaseURL = "https://ai-gateway.vercel.sh/v1"
)

// Question is one typed question. gotorque sends only "boolean" questions, whose
// criteria define what the true and false answers mean.
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Request asks every question against the same state in one round trip.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one boolean answer: the probability that the answer is yes.
type Answer struct {
	Type        string  `json:"type"`
	Probability float64 `json:"probability"`
}

// Usage is the gateway's token accounting for one request.
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// Response carries one answer per question id.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Evaluator answers typed questions. Client reaches the gateway; Stub answers
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

// Client calls the gateway's native evaluation endpoint. That endpoint is not
// reachable through the OpenAI-compatible API the other roles use.
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
		return base + "/evaluate"
	}
	return DefaultBaseURL + "/evaluate"
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Jev answers in about half a second; a minute covers a slow gateway
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
		return Response{}, retryable(httpResp.StatusCode), fmt.Errorf("gateway returned HTTP %d for Jev: %s", httpResp.StatusCode, gatewayMessage(data))
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Response{}, false, fmt.Errorf("decode Jev response: %w", err)
	}
	return resp, false, nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// gatewayMessage extracts the gateway's own error text. It is what an operator
// needs to act: "requires a valid credit card on file" and "Zero Data Retention
// is only available for Pro and Enterprise plans" both arrive this way.
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

// Preflight spends one minimal request to prove the key, the account's billing
// state, and the model all work before a campaign starts repository work. A new
// gateway account refuses every request until a card is on file, and learning
// that at the analyst node would cost the whole baseline first.
func (c Client) Preflight(ctx context.Context) error {
	resp, err := c.Evaluate(ctx, Request{
		State:     "gotorque connectivity check",
		Questions: map[string]Question{"ok": {Type: "boolean", Instructions: "Is this a connectivity check?"}},
	})
	if err != nil {
		return fmt.Errorf("preflight against Jev: %w", err)
	}
	if _, ok := resp.Answers["ok"]; !ok {
		return errors.New("preflight against Jev: response carried no answer")
	}
	return nil
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
	return Response{Model: Model, Answers: answers}, nil
}
