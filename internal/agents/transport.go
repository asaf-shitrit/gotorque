package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// eventStreamTransport hardens the event streams model calls read. Neither
// openai-go nor ADK exposes a hook between the socket and the SSE decoder, so
// the body is filtered here, below both: see eventStreamReader for what it
// removes and ErrStreamIncomplete for what it adds.
type eventStreamTransport struct {
	base http.RoundTripper
}

func (t eventStreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || !isEventStream(resp) {
		return resp, err
	}
	resp.Body = newEventStreamReader(resp.Body)
	// Dropped keepalives shorten the body, so a declared length would lie.
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// isEventStream selects the bodies eventStreamReader may filter. An error
// status is left alone even when it claims to be a stream: openai-go reads
// that body to build the status error the ladder in fence.go classifies, and
// a stream error raised while it reads would mask the status.
func isEventStream(resp *http.Response) bool {
	if !httpOK(resp.StatusCode) {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// reasoningTransport sets reasoning.effort on every Responses API request one
// role makes.
//
// It exists because ADK's openaimodel maps only MaxOutputTokens from genai's
// config onto the Responses API request and never maps ThinkingConfig, so
// there is no field on the ADK side that reaches reasoning.effort. The field
// shape is openai-go's ResponseNewParams.Reasoning (shared.ReasoningParam),
// which serializes as {"reasoning":{"effort":"..."}}.
type reasoningTransport struct {
	base   http.RoundTripper
	effort ReasoningEffort
}

func (t reasoningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil || req.Body == http.NoBody || !strings.HasSuffix(req.URL.Path, "/responses") {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body to set reasoning effort: %w", err)
	}
	// Failing is deliberate: an operator who set an effort must not get a
	// campaign that silently ran at the provider's default.
	body, err = withReasoningEffort(body, t.effort)
	if err != nil {
		return nil, fmt.Errorf("set reasoning effort %q: %w", t.effort, err)
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(out)
}

// withReasoningEffort returns body with reasoning.effort set, keeping every
// other field's bytes, including any other reasoning setting, as they were.
func withReasoningEffort(body []byte, effort ReasoningEffort) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var reasoning map[string]json.RawMessage
	if existing, ok := fields["reasoning"]; ok {
		if err := json.Unmarshal(existing, &reasoning); err != nil {
			return nil, err
		}
	}
	if reasoning == nil {
		reasoning = map[string]json.RawMessage{}
	}
	value, err := json.Marshal(effort)
	if err != nil {
		return nil, err
	}
	reasoning["effort"] = value
	if fields["reasoning"], err = marshalVerbatim(reasoning); err != nil {
		return nil, err
	}
	return marshalVerbatim(fields)
}

// marshalVerbatim encodes without HTML escaping. Prompts are full of Go
// source, and json.Marshal would rewrite every & and < in them.
func marshalVerbatim(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
