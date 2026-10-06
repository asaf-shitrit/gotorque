package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
)

// maxProviderMessage bounds the provider's own message in a summarized
// error, in runes.
const maxProviderMessage = 300

// providerError is an endpoint's HTTP error reduced to what a person acts on:
// the status and the provider's own message. It still unwraps to the
// openai-go error, so errors.As and status classification see the original.
//
// openai-go's Error() is the request line plus the raw response body. On an
// OpenRouter 402 that body carried 29 previous_errors, each repeating "You
// requested up to 32768 tokens, but can only afford N", and the 7 KB string
// was copied into the stop reason, the campaign error and every degraded-role
// record: 28 KB of a 111 KB report.json, and scorecard rows nobody could read
// (#87).
type providerError struct {
	summary string
	err     error
}

func (e *providerError) Error() string { return e.summary }
func (e *providerError) Unwrap() error { return e.err }

// summarizeAPIError returns err with an openai-go API error summarized as
// "HTTP <status> <text>: <provider message>", or err unchanged when it carries
// none.
func summarizeAPIError(err error) error {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	summary := fmt.Sprintf("HTTP %d %s", apiErr.StatusCode, http.StatusText(apiErr.StatusCode))
	if message := providerMessage(apiErr); message != "" {
		summary += ": " + message
	}
	return &providerError{summary: summary, err: err}
}

// providerMessage is the human-readable message of an API error body.
// OpenRouter puts it at the top level ({"message": ...}) and OpenAI-shaped
// endpoints under "error"; anything else falls back to the decoded field, then
// to the raw body, cut to maxProviderMessage.
func providerMessage(apiErr *openai.Error) string {
	raw := apiErr.RawJSON()
	var body struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	message := ""
	if json.Unmarshal([]byte(raw), &body) == nil {
		message = firstNonEmpty(body.Message, body.Error.Message)
	}
	message = firstNonEmpty(message, apiErr.Message, raw)
	return truncateRunes(strings.TrimSpace(message), maxProviderMessage)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
