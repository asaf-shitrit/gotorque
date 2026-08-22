package agents

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
	require.ErrorContains(t, provider.ValidateConnectivity(context.Background()), "OPENAI_API_KEY")
}
