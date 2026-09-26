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
	// EnvAllowDrift, set to any non-empty value, downgrades the release-date
	// and canary drift guards in Preflight from a failure to a warning. It
	// does not affect the provider pin below: a wrong provider is refused
	// unconditionally, since nothing recorded is valid for a provider other
	// than TypeSafe's own.
	EnvAllowDrift = "GOTORQUE_JEV_ALLOW_DRIFT"
	// pinnedProvider is the only gateway provider gotorque accepts an answer
	// from. The gateway serves the "typesafe-ai/jev" alias from more than one
	// upstream: a live probe (see ADR 0029) found requests also routed to
	// "digitalocean", with typesafe-ai answering only as its fallback.
	// Nothing confirms that route runs the same build TypeSafe serves, so
	// every baseline and canary in this package is only valid for
	// pinnedProvider.
	pinnedProvider = "typesafe-ai"
)

// Question is one typed question. gotorque sends only "boolean" questions, whose
// criteria define what the true and false answers mean.
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Request asks every question against the same state in one round trip.
// ProviderOptions is filled in by Evaluate when the caller leaves it nil; a
// caller never needs to set it.
type Request struct {
	Model           string              `json:"model"`
	State           any                 `json:"state"`
	Questions       map[string]Question `json:"questions"`
	ProviderOptions *ProviderOptions    `json:"providerOptions,omitempty"`
}

// ProviderOptions restricts which upstream serves a request. See pinnedProvider.
type ProviderOptions struct {
	Gateway GatewayOptions `json:"gateway"`
}

// GatewayOptions is the gateway-specific slice of ProviderOptions the
// AI Gateway's evaluation modality documents.
type GatewayOptions struct {
	Only []string `json:"only,omitempty"`
}

// pinnedProviderOptions restricts every request this client sends to
// pinnedProvider, unless a caller already set its own ProviderOptions.
func pinnedProviderOptions() *ProviderOptions {
	return &ProviderOptions{Gateway: GatewayOptions{Only: []string{pinnedProvider}}}
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
	Model            string            `json:"model"`
	Answers          map[string]Answer `json:"answers"`
	Usage            Usage             `json:"usage"`
	ProviderMetadata *ProviderMetadata `json:"providerMetadata,omitempty"`
}

// ProviderMetadata carries the gateway's own record of how a request was
// routed, alongside the answer.
type ProviderMetadata struct {
	Gateway *GatewayMetadata `json:"gateway,omitempty"`
}

// GatewayMetadata is the gateway-specific slice of ProviderMetadata.
type GatewayMetadata struct {
	Routing *GatewayRouting `json:"routing,omitempty"`
}

// GatewayRouting is what the gateway reports about how it routed one
// request. FinalProvider is the provider whose answer the response actually
// carries; it can differ from the plan when a preferred provider fails over
// to another (see pinnedProvider).
type GatewayRouting struct {
	FinalProvider string `json:"finalProvider,omitempty"`
}

// finalProvider reports which provider actually answered, or "" when the
// gateway did not report it.
func (r Response) finalProvider() string {
	if r.ProviderMetadata == nil || r.ProviderMetadata.Gateway == nil || r.ProviderMetadata.Gateway.Routing == nil {
		return ""
	}
	return r.ProviderMetadata.Gateway.Routing.FinalProvider
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
	if req.ProviderOptions == nil {
		req.ProviderOptions = pinnedProviderOptions()
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
	if final := resp.finalProvider(); final != "" && final != pinnedProvider {
		return Response{}, false, fmt.Errorf("answered through provider %q, not %q; refusing an answer the baseline was never measured against", final, pinnedProvider)
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

// Preflight spends one minimal request to prove the key, the account's
// billing state, and the model all work before a campaign starts repository
// work. A new gateway account refuses every request until a card is on file,
// and learning that at the analyst node would cost the whole baseline first.
// That one request carries the canary questions (canary.go) instead of a
// plain connectivity check, so the same call also catches a Jev version
// behind the gateway's unversioned alias moving enough to matter.
//
// Preflight also makes one free GET against the models listing to compare
// its release_date against baselineModelRelease. Both that check and the
// canary check fail the preflight on a mismatch; EnvAllowDrift downgrades
// both to a warning, returned alongside a nil error. An unreachable listing
// only ever warns: it is not evidence of drift, just of not knowing.
func (c Client) Preflight(ctx context.Context) ([]string, error) {
	resp, err := c.Evaluate(ctx, Request{State: canaryState, Questions: canaryQuestionSet()})
	if err != nil {
		return nil, fmt.Errorf("preflight against Jev: %w", err)
	}
	if len(resp.Answers) == 0 {
		return nil, errors.New("preflight against Jev: response carried no answer")
	}
	var warnings []string
	if msg, hardFail := c.releaseCheck(ctx); msg != "" {
		if err := driftGuard(msg, hardFail, &warnings); err != nil {
			return warnings, err
		}
	}
	if drift := canaryDrift(resp.Answers); len(drift) > 0 {
		msg := fmt.Sprintf("Jev's canary answers moved: %s; re-measure with TestLiveCanary", strings.Join(drift, "; "))
		if err := driftGuard(msg, true, &warnings); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// driftGuard applies EnvAllowDrift to one preflight finding: msg is appended
// as a warning either when hardFail is false (an unreachable listing is
// never a hard failure) or when the override is set; otherwise msg becomes
// the preflight's error.
func driftGuard(msg string, hardFail bool, warnings *[]string) error {
	if !hardFail || os.Getenv(EnvAllowDrift) != "" {
		*warnings = append(*warnings, msg)
		return nil
	}
	return fmt.Errorf("%s (set %s=1 to run anyway)", msg, EnvAllowDrift)
}

// releaseCheck compares the gateway's listed release_date for "jev" against
// baselineModelRelease. hardFail is false for an unreachable listing (a
// warning either way) and true for a listed date that disagrees with the
// baseline.
func (c Client) releaseCheck(ctx context.Context) (msg string, hardFail bool) {
	release, err := c.modelRelease(ctx)
	if err != nil {
		return fmt.Sprintf("could not check Jev's model release date: %v", err), false
	}
	if release == baselineModelRelease {
		return "", false
	}
	return fmt.Sprintf("Jev's listed release_date is %q, the baseline was measured against %q; re-measure with TestLiveBaseline", release, baselineModelRelease), true
}

// modelsListing is the shape of GET <base>/typesafe/v1/models.
type modelsListing struct {
	Models []struct {
		Name        string `json:"name"`
		ReleaseDate string `json:"release_date"`
	} `json:"models"`
}

// modelsEndpoint derives the TypeSafe-compatible models listing URL from the
// configured evaluate endpoint's base, e.g.
// "https://ai-gateway.vercel.sh/v1" -> "https://ai-gateway.vercel.sh/typesafe/v1/models".
func (c Client) modelsEndpoint() string {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/v1")
	return base + "/typesafe/v1/models"
}

// modelRelease fetches the "jev" entry's release_date. The listing is free
// (no tokens spent), so this is safe to call on every preflight.
func (c Client) modelRelease(ctx context.Context) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsEndpoint(), nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("list Jev models: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read Jev models listing: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return "", fmt.Errorf("gateway returned HTTP %d for the Jev models listing: %s", httpResp.StatusCode, gatewayMessage(data))
	}
	var listing modelsListing
	if err := json.Unmarshal(data, &listing); err != nil {
		return "", fmt.Errorf("decode Jev models listing: %w", err)
	}
	for _, m := range listing.Models {
		if m.Name == "jev" {
			return m.ReleaseDate, nil
		}
	}
	return "", errors.New(`models listing carried no "jev" entry`)
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
