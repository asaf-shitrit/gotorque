package agents

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// capturingTransport records the body of the request it receives.
type capturingTransport struct{ body string }

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		c.body = string(data)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func sendThrough(t *testing.T, method, url, body string) string {
	t.Helper()
	base := &capturingTransport{}
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := providerTransport{base: base}.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return base.body
}

func TestProviderTransportSortsOpenRouterByThroughput(t *testing.T) {
	got := sendThrough(t, http.MethodPost, "https://openrouter.ai/api/v1/responses", `{"model":"m","input":"a<b & c"}`)
	require.JSONEq(t, `{"model":"m","input":"a<b & c","provider":{"sort":"throughput"}}`, got)
	require.Contains(t, got, "a<b & c", "prompt bytes stay unescaped")
}

func TestProviderTransportLeavesOtherRequestsAlone(t *testing.T) {
	body := `{"model":"m","input":"x"}`
	require.Equal(t, body, sendThrough(t, http.MethodPost, "https://api.example.com/v1/responses", body), "another endpoint")
	require.Equal(t, body, sendThrough(t, http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", body), "another path")
	require.Empty(t, sendThrough(t, http.MethodGet, "https://openrouter.ai/api/v1/models", ""), "a GET")
	kept := `{"model":"m","provider":{"only":["Together"]}}`
	require.Equal(t, kept, sendThrough(t, http.MethodPost, "https://openrouter.ai/api/v1/responses", kept), "an explicit preference")
}

func TestProviderTransportRejectsAnUnreadableBody(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://openrouter.ai/api/v1/responses", strings.NewReader("not json"))
	require.NoError(t, err)
	resp, err := providerTransport{base: &capturingTransport{}}.RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorContains(t, err, "set provider routing")
	require.True(t, isOpenRouter("eu.openrouter.ai"))
	require.False(t, isOpenRouter("openrouter.ai.evil.com"))
}
