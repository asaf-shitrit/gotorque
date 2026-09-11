package agents

import (
	"context"
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
	// A nil client would leave the SDK with an unbounded one, so a stalled
	// endpoint would burn the whole agent deadline instead of erroring.
	client := OpenAIProvider{}.httpClient()
	require.NotNil(t, client)
	require.Equal(t, requestTimeout, client.Timeout)

	supplied := &http.Client{}
	require.Same(t, supplied, OpenAIProvider{Client: supplied}.httpClient())
}

// The agent deadline must fit every retry fence.go performs, or a stalled
// endpoint still ends the campaign instead of being retried.
func TestRequestTimeoutFitsInsideAgentDeadline(t *testing.T) {
	const attempts = 4
	const backoff = 15*time.Second + 30*time.Second + 60*time.Second
	if worst := attempts*requestTimeout + backoff; worst > 20*time.Minute {
		t.Errorf("worst-case role call = %s, want <= the 20m agent deadline", worst)
	}
}
