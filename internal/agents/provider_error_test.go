package agents

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
)

// openRouter402 is the shape of the body OpenRouter sent on 2026-10-03 when
// the balance ran out, cut to three of its 29 previous_errors.
const openRouter402 = `{"message":"This request requires more credits, or fewer max_tokens. You requested up to 32768 tokens, but can only afford 1025.","code":402,"metadata":{"limit_source":"openrouter_credits","previous_errors":[{"code":402,"message":"You requested up to 32768 tokens, but can only afford 578."},{"code":402,"message":"You requested up to 32768 tokens, but can only afford 462."},{"code":402,"message":"You requested up to 32768 tokens, but can only afford 385."}]}}`

func apiErrorWithBody(t *testing.T, status int, body string) *openai.Error {
	t.Helper()
	apiErr := newAPIError(status)
	require.NoError(t, apiErr.UnmarshalJSON([]byte(body)))
	return apiErr
}

// TestSummarizeAPIErrorKeepsTheMessageAndDropsTheBody pins #87: the error a
// campaign records names the status and the provider's message, not the raw
// body, while classification still sees the original error.
func TestSummarizeAPIErrorKeepsTheMessageAndDropsTheBody(t *testing.T) {
	apiErr := apiErrorWithBody(t, http.StatusPaymentRequired, openRouter402)
	require.Contains(t, apiErr.Error(), "previous_errors", "the SDK's own message carries the whole body")

	got := summarizeAPIError(fmt.Errorf("openai: call failed: %w", apiErr))
	require.Equal(t, "HTTP 402 Payment Required: This request requires more credits, or fewer max_tokens. You requested up to 32768 tokens, but can only afford 1025.", got.Error())
	var unwrapped *openai.Error
	require.ErrorAs(t, got, &unwrapped)
	require.Equal(t, http.StatusPaymentRequired, unwrapped.StatusCode)
}

func TestProviderMessageReadsEitherBodyShape(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{name: "OpenRouter top-level message", body: `{"message":"out of credits","code":402}`, want: "out of credits"},
		{name: "OpenAI error object", body: `{"error":{"message":"invalid key","type":"auth"}}`, want: "invalid key"},
		{name: "unrecognised body is cut short", body: `{"detail":"` + strings.Repeat("x", 400) + `"}`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := providerMessage(apiErrorWithBody(t, http.StatusBadRequest, tc.body))
			if tc.want != "" {
				require.Equal(t, tc.want, got)
				return
			}
			require.LessOrEqual(t, len([]rune(got)), maxProviderMessage+1)
			require.True(t, strings.HasSuffix(got, "…"))
		})
	}
}

func TestSummarizeAPIErrorLeavesOtherErrorsAlone(t *testing.T) {
	plain := errors.New("model stream ended without a completed response")
	require.Same(t, plain, summarizeAPIError(plain))
}
