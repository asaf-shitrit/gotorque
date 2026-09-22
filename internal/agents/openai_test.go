package agents

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProviderValidatesEndpointAndModelsWithoutPersistingCredentials(t *testing.T) {
	routing := Routing{RoleCoordinator: "sol", RoleExplorer: "luna", RoleAnalyst: "terra", RoleOptimizer: "sol", RoleReviewer: "terra"}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"sol"},{"id":"luna"},{"id":"terra"}]}`)), Header: make(http.Header), Request: r}, nil
	})}
	provider := OpenAIProvider{APIKey: "secret", BaseURL: "https://example.test/v1", Routing: routing, Client: client}
	require.NoError(t, provider.ValidateConnectivity(context.Background()))
}

// catalogueProvider serves one /models document to ValidateConnectivity.
func catalogueProvider(catalogue string) OpenAIProvider {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(catalogue)), Header: make(http.Header), Request: r}, nil
	})}
	routing := Routing{RoleCoordinator: "sol", RoleExplorer: "luna", RoleAnalyst: "terra", RoleOptimizer: "sol", RoleReviewer: "terra"}
	return OpenAIProvider{APIKey: "secret", BaseURL: "https://example.test/v1", Routing: routing, Client: client}
}

// A request for more completion tokens than the model allows cannot succeed,
// and without the check the campaign learned that only at its first role
// call, after discovery had already run.
func TestOpenAIProviderRejectsAModelBelowTheOutputBudget(t *testing.T) {
	provider := catalogueProvider(`{"data":[
		{"id":"sol","top_provider":{"max_completion_tokens":400000}},
		{"id":"luna","top_provider":{"max_completion_tokens":16384}},
		{"id":"terra","top_provider":{"max_completion_tokens":32768}}]}`)
	err := provider.ValidateConnectivity(context.Background())
	require.ErrorContains(t, err, `"luna"`)
	require.ErrorContains(t, err, string(RoleExplorer))
	require.ErrorContains(t, err, "16384")
}

// Not every endpoint advertises a ceiling, and OpenRouter's router models
// report it as null. A missing value is no evidence of a low one.
func TestOpenAIProviderAcceptsModelsWithoutAnAdvertisedCeiling(t *testing.T) {
	provider := catalogueProvider(`{"data":[
		{"id":"sol"},
		{"id":"luna","top_provider":{"max_completion_tokens":null}},
		{"id":"terra","context_length":163840,"top_provider":{"context_length":163840,"max_completion_tokens":0}}]}`)
	require.NoError(t, provider.ValidateConnectivity(context.Background()))
}

func TestOpenAIProviderRejectsAnInvalidReasoningEffortBeforeTheNetwork(t *testing.T) {
	contacted := false
	provider := OpenAIProvider{APIKey: "secret", Routing: DefaultRouting(), Reasoning: Reasoning{RoleOptimizer: "extreme"}, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		contacted = true
		return nil, errors.New("unexpected request")
	})}}
	require.ErrorContains(t, provider.ValidateConnectivity(context.Background()), EnvOptimizerReasoning)
	require.False(t, contacted, "an invalid effort must fail before the endpoint is contacted")
}

func TestOpenAIProviderRejectsAnUnadvertisedModel(t *testing.T) {
	err := catalogueProvider(`{"data":[{"id":"sol"},{"id":"luna"}]}`).ValidateConnectivity(context.Background())
	require.ErrorContains(t, err, `configured model "terra" for analyst is not advertised`)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenAIProviderRejectsMissingCredential(t *testing.T) {
	provider := NewOpenAIProviderFromEnvironment()
	provider.APIKey = ""
	require.ErrorContains(t, provider.ValidateConnectivity(context.Background()), EnvAPIKey)
}

func TestOpenAIProviderEndpointDefaultsToOpenRouter(t *testing.T) {
	provider := OpenAIProvider{}
	require.Equal(t, DefaultOpenRouterBaseURL, provider.endpoint())

	provider.BaseURL = "https://example.test/v1/"
	require.Equal(t, "https://example.test/v1", provider.endpoint())
}

func TestOpenAIProviderSuppliesBoundedHTTPClient(t *testing.T) {
	// The client must be supplied so the SDK never falls back to its own, and it
	// must carry no whole-request timeout: with streaming, silence is what
	// streamIdleTimeout bounds, and a total-duration bound cut calls that were
	// still producing. The header timeout keeps a dead endpoint from hanging
	// before any byte arrives.
	client := OpenAIProvider{}.httpClient()
	require.NotNil(t, client)
	require.Zero(t, client.Timeout, "a whole-request timeout cuts working generations")
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotZero(t, transport.ResponseHeaderTimeout)
	require.NotZero(t, transport.TLSHandshakeTimeout)

	supplied := &http.Client{}
	require.Same(t, supplied, OpenAIProvider{Client: supplied}.httpClient())
}

// The agent deadline must fit every retry fence.go performs, or a stalled
// endpoint still ends the campaign instead of being retried.
func TestRequestTimeoutFitsInsideAgentDeadline(t *testing.T) {
	const attempts = 4
	const backoff = 15*time.Second + 30*time.Second + 60*time.Second
	if worst := attempts*attemptTimeout + backoff; worst > 20*time.Minute {
		t.Errorf("worst-case role call = %s, want <= the 20m agent deadline", worst)
	}
}

// A stall now costs a fraction of an attempt: the idle bound is what fails a
// silent stream, and it has to leave room for the rest of the ladder.
func TestStreamIdleTimeoutLeavesRoomForTheLadder(t *testing.T) {
	if streamIdleTimeout >= attemptTimeout {
		t.Fatalf("idle bound %s must be shorter than the %s attempt", streamIdleTimeout, attemptTimeout)
	}
	if attempts := 4; time.Duration(attempts)*streamIdleTimeout > 20*time.Minute {
		t.Fatalf("a fully stalled ladder would exceed the 20m agent deadline")
	}
}
