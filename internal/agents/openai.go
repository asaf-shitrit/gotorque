package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

// EnvAPIKey and EnvBaseURL name the only credential and endpoint inputs. The
// wire protocol is OpenAI-compatible, but the endpoint is OpenRouter: nothing
// here reads OPENAI_API_KEY, so an OpenAI key left in the shell is never used
// and an unset base URL cannot fall back to api.openai.com.
const (
	EnvAPIKey                = "OPENROUTER_API_KEY" //nolint:gosec // environment variable name, not a credential value
	EnvBaseURL               = "OPENROUTER_BASE_URL"
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
)

type OpenAIProvider struct {
	APIKey  string
	BaseURL string
	Routing Routing
	Client  *http.Client

	// Reasoning sets reasoning.effort per role; a role left out sends none.
	Reasoning Reasoning

	// Usage accumulates per-role token usage across every model this provider
	// decorates. It is shared by pointer when the value struct is copied.
	Usage *UsageCollector

	// Observer, when set, receives one CallInfo per model-call attempt, so a
	// campaign can report which role is waiting and for how long.
	Observer CallObserver
}

func NewOpenAIProviderFromEnvironment() OpenAIProvider {
	return OpenAIProvider{APIKey: os.Getenv(EnvAPIKey), BaseURL: os.Getenv(EnvBaseURL), Routing: RoutingFromEnvironment(), Reasoning: ReasoningFromEnvironment(), Usage: NewUsageCollector()}
}

// endpoint resolves the base URL every caller must use. It exists so model
// calls and the connectivity preflight cannot disagree: passing an empty
// BaseURL to the OpenAI SDK silently targets api.openai.com, which sends the
// OpenRouter key to the wrong provider and fails with 401 only after the
// preflight has already passed.
func (p OpenAIProvider) endpoint() string {
	if base := strings.TrimRight(p.BaseURL, "/"); base != "" {
		return base
	}
	return DefaultOpenRouterBaseURL
}

func (p OpenAIProvider) ModelFor(ctx context.Context, role Role) (model.LLM, error) {
	inner, err := p.roleModel(ctx, role)
	if err != nil {
		return nil, err
	}
	// Streaming sits inside the fence so the fence still sees one response per
	// attempt, and the endpoint still sends bytes while the model works.
	return NewFenceStrippingModel(newStreamedModel(inner), string(role), p.Usage, p.Observer), nil
}

// roleModel returns one role's endpoint model before any decoration.
func (p OpenAIProvider) roleModel(ctx context.Context, role Role) (model.LLM, error) {
	if err := p.Routing.Validate(); err != nil {
		return nil, err
	}
	if err := p.Reasoning.Validate(); err != nil {
		return nil, err
	}
	return openaimodel.NewModel(ctx, p.Routing[role], &openaimodel.ClientConfig{APIKey: p.APIKey, BaseURL: p.endpoint(), HTTPClient: p.modelClient(role)})
}

// attemptTimeout bounds one attempt of the retry ladder in fence.go. It is the
// per-attempt deadline, and it must leave room for the whole ladder inside the
// orchestrator's per-node agent deadline (attempts×attemptTimeout + backoff <
// AgentTimeout), otherwise a stalled provider kills the node mid-ladder and
// takes the campaign with it.
//
// Stalls themselves are caught much sooner than this by streamIdleTimeout:
// streaming makes idleness measurable, so a call that goes quiet fails in two
// minutes instead of consuming the whole attempt. The attempt budget therefore
// only ever cuts a call that is still producing output, and it was four
// minutes until the optimizer, even at low effort, was measured writing 10-12k
// completion tokens per call: completed calls across nine live campaigns took
// up to 3m43s, and on gojq a whole cycle's attempts were cut at 4m0s and the
// breaker ended the campaign. Six minutes, with one attempt fewer, keeps the
// ladder inside the node deadline (TestRequestTimeoutFitsInsideAgentDeadline).
const attemptTimeout = 6 * time.Minute

// httpClient returns the client model calls are built on; modelClient layers
// the per-role transports over it.
//
// It carries no whole-request timeout on purpose. That bound was the old
// instrument for a non-streaming call, where silence is the only observable,
// and it could not tell a slow generation from a dead connection: it cut
// calls that were still working at four minutes and reported them as stalls.
// The stream now reports progress, so streamIdleTimeout bounds silence and the
// attempt deadline in fence.go bounds the call.
func (p OpenAIProvider) httpClient() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// modelClient returns the client one role's model calls use: httpClient's,
// with every event stream filtered by eventStreamTransport and, when the role
// has an effort configured, every request carrying it. An injected Client is
// copied rather than modified, so a caller sharing it sees no change.
func (p OpenAIProvider) modelClient(role Role) *http.Client {
	client := *p.httpClient()
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var transport http.RoundTripper = eventStreamTransport{base: base}
	if effort := p.Reasoning[role]; effort != "" {
		transport = reasoningTransport{base: transport, effort: effort}
	}
	client.Transport = transport
	return &client
}

// UsageReporter exposes the shared token-usage collector so the campaign
// layer can read per-role totals after a run.
func (p OpenAIProvider) UsageReporter() *UsageCollector { return p.Usage }

// ValidateConnectivity checks credentials, endpoint reachability, model IDs,
// reasoning settings, and each model's completion ceiling before a campaign
// starts expensive repository work. It never records the API key or response
// body in campaign state.
func (p OpenAIProvider) ValidateConnectivity(ctx context.Context) error {
	if p.APIKey == "" {
		return errors.New(EnvAPIKey + " is required for --adk")
	}
	if err := p.Routing.Validate(); err != nil {
		return err
	}
	if err := p.Reasoning.Validate(); err != nil {
		return err
	}
	available, err := p.listEndpointModels(ctx)
	if err != nil {
		return err
	}
	for _, role := range AllRoles {
		if err := checkAdvertised(role, p.Routing[role], available); err != nil {
			return err
		}
	}
	return nil
}

// advertisedModel is the part of an endpoint's /models entry the preflight
// reads. OpenRouter reports the completion ceiling under top_provider; an
// absent or null value decodes to zero and is not checked.
type advertisedModel struct {
	ID          string `json:"id"`
	TopProvider struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

// checkAdvertised fails a role whose model the endpoint does not list, or
// whose advertised completion ceiling is below MaxOutputTokens. A model
// capped below that budget cannot deliver the output the budget was sized
// for, and learning so from the first role call wastes the discovery work the
// campaign did before it.
func checkAdvertised(role Role, id string, available map[string]advertisedModel) error {
	advertised, ok := available[id]
	if !ok {
		return fmt.Errorf("configured model %q for %s is not advertised by endpoint", id, role)
	}
	if ceiling := advertised.TopProvider.MaxCompletionTokens; ceiling > 0 && ceiling < MaxOutputTokens {
		return fmt.Errorf("configured model %q for %s allows at most %d completion tokens, below the %d every role requests", id, role, ceiling, MaxOutputTokens)
	}
	return nil
}

func httpOK(code int) bool {
	return code >= 200 && code < 300
}

func (p OpenAIProvider) listEndpointModels(ctx context.Context) (map[string]advertisedModel, error) {
	base := p.endpoint()
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenRouter endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !httpOK(resp.StatusCode) {
		return nil, fmt.Errorf("OpenRouter endpoint returned HTTP %s", resp.Status)
	}
	var document struct {
		Data []advertisedModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	available := make(map[string]advertisedModel, len(document.Data))
	for _, item := range document.Data {
		available[item.ID] = item
	}
	if len(available) == 0 {
		return nil, errors.New("OpenRouter endpoint returned no models")
	}
	return available, nil
}
