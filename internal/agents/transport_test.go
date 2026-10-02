package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// fakeEndpoint is an httptest stand-in for OpenRouter's Responses API. It
// records the body of every request and answers with a fixed reply.
type fakeEndpoint struct {
	mu          sync.Mutex
	bodies      [][]byte
	contentType string
	status      int
	reply       string
}

func (e *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	e.bodies = append(e.bodies, body)
	e.mu.Unlock()
	w.Header().Set("Content-Type", e.contentType)
	w.WriteHeader(e.status)
	_, _ = io.WriteString(w, e.reply)
}

func (e *fakeEndpoint) lastBody(t *testing.T) map[string]any {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.NotEmpty(t, e.bodies, "the endpoint received no request")
	var body map[string]any
	require.NoError(t, json.Unmarshal(e.bodies[len(e.bodies)-1], &body))
	return body
}

func serveFakeEndpoint(t *testing.T, e *fakeEndpoint) OpenAIProvider {
	t.Helper()
	if e.status == 0 {
		e.status = http.StatusOK
	}
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	return OpenAIProvider{APIKey: "secret", BaseURL: server.URL + "/v1", Model: "test/model"}
}

func sseFakeEndpoint(reply string) *fakeEndpoint {
	return &fakeEndpoint{contentType: "text/event-stream", reply: reply}
}

// callOptimizerModel runs one call on the undecorated optimizer model, the layer ADK
// hands straight to openai-go, and returns the answer text it produced.
func callOptimizerModel(t *testing.T, p OpenAIProvider, stream bool) (string, error) {
	t.Helper()
	llm, err := p.endpointModel(context.Background())
	require.NoError(t, err)
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "answer & <return> json"}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: MaxOutputTokens},
	}
	var final string
	for resp, err := range llm.GenerateContent(context.Background(), req, stream) {
		if err != nil {
			return final, err
		}
		if resp != nil && !resp.Partial && resp.Content != nil {
			final = strings.Join(answerText(resp), "")
		}
	}
	return final, nil
}

// OpenRouter documents SSE comments as keepalives. openai-go dispatched the
// comment's blank line as an event with empty data and failed the whole call
// with `unexpected end of JSON input`.
func TestModelCallSurvivesEndpointKeepalives(t *testing.T) {
	e := sseFakeEndpoint(sseKeepalive + sseCreated + sseKeepalive + sseItemAdded + sseDeltaOne + sseKeepalive + sseDeltaTwo + sseCompleted)
	got, err := callOptimizerModel(t, serveFakeEndpoint(t, e), true)
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, got)
}

// A stream that ends cleanly before response.completed used to yield its
// partial text as the final answer, with a nil error.
func TestModelCallFailsAStreamCutBeforeCompletion(t *testing.T) {
	e := sseFakeEndpoint(sseCreated + sseItemAdded + sseDeltaOne)
	_, err := callOptimizerModel(t, serveFakeEndpoint(t, e), true)
	require.ErrorIs(t, err, ErrStreamIncomplete)
}

// ADK ignores response.incomplete, so an answer cut at max_output_tokens was
// yielded as if it were whole.
func TestModelCallFailsAnAnswerCutAtTheTokenLimit(t *testing.T) {
	e := sseFakeEndpoint(sseCreated + sseItemAdded + sseDeltaOne + sseIncomplete)
	_, err := callOptimizerModel(t, serveFakeEndpoint(t, e), true)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	require.ErrorContains(t, err, "max_output_tokens")
}

// The same failure seen through the streaming decorator a campaign uses: the
// cut stream is an error the ladder can retry, not a response.
func TestStreamedModelSurfacesACutStreamAsAnError(t *testing.T) {
	p := serveFakeEndpoint(t, sseFakeEndpoint(sseCreated+sseItemAdded+sseDeltaOne+sseDeltaTwo))
	inner, err := p.endpointModel(context.Background())
	require.NoError(t, err)
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "go"}}}}}
	for resp, err := range newStreamedModel(inner).GenerateContent(context.Background(), req, false) {
		require.Nil(t, resp)
		require.ErrorIs(t, err, ErrStreamIncomplete)
	}
}

func TestModelCallLeavesNonStreamingResponsesAlone(t *testing.T) {
	e := &fakeEndpoint{contentType: "application/json", reply: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"{\"ok\":true}","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`}
	got, err := callOptimizerModel(t, serveFakeEndpoint(t, e), false)
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, got)
}

// A client error status must reach the retry ladder as a status error, even
// when the body claims to be a stream.
func TestModelCallKeepsTheStatusOfAFailedStreamRequest(t *testing.T) {
	e := &fakeEndpoint{contentType: "text/event-stream", status: http.StatusBadRequest, reply: `{"error":{"message":"bad model","type":"invalid_request_error"}}`}
	_, err := callOptimizerModel(t, serveFakeEndpoint(t, e), true)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrStreamIncomplete)
	require.ErrorContains(t, err, "400")
}

func TestModelCallSendsTheConfiguredReasoningEffort(t *testing.T) {
	e := sseFakeEndpoint(sseCreated + sseItemAdded + sseDeltaOne + sseDeltaTwo + sseCompleted)
	p := serveFakeEndpoint(t, e)
	p.Reasoning = ReasoningHigh

	_, err := callOptimizerModel(t, p, true)
	require.NoError(t, err)
	body := e.lastBody(t)
	require.Equal(t, map[string]any{"effort": "high"}, body["reasoning"])
	// The rest of the request is ADK's, untouched.
	require.Equal(t, "test/model", body["model"])
	require.Equal(t, true, body["stream"])
	require.InDelta(t, MaxOutputTokens, body["max_output_tokens"], 0)

	// Without an effort the request carries no reasoning field at all, which
	// is the behavior before the knob existed.
	p.Reasoning = ""
	_, err = callOptimizerModel(t, p, true)
	require.NoError(t, err)
	require.NotContains(t, e.lastBody(t), "reasoning")
}

func TestOptimizerModelRejectsAnInvalidReasoningEffort(t *testing.T) {
	p := serveFakeEndpoint(t, sseFakeEndpoint(""))
	p.Reasoning = "max"
	_, err := p.endpointModel(context.Background())
	require.ErrorContains(t, err, EnvOptimizerReasoning)
	_, err = p.OptimizerModel(context.Background())
	require.ErrorContains(t, err, EnvOptimizerReasoning)
}

func TestOptimizerModelRequiresAModelID(t *testing.T) {
	_, err := OpenAIProvider{APIKey: "secret"}.OptimizerModel(context.Background())
	require.ErrorContains(t, err, "optimizer model ID is required")
}

func TestModelClientWrapsWithoutMutatingAnInjectedClient(t *testing.T) {
	injected := &http.Client{}
	p := OpenAIProvider{Client: injected, Reasoning: ReasoningLow}

	withEffortClient := p.modelClient()
	require.NotSame(t, injected, withEffortClient)
	require.Nil(t, injected.Transport, "the injected client must not be modified")
	withEffort, ok := withEffortClient.Transport.(reasoningTransport)
	require.True(t, ok)
	require.Equal(t, ReasoningLow, withEffort.effort)
	off, ok := withEffort.base.(reasoningOffTransport)
	require.True(t, ok, "reasoning-off runs after the effort is set, so it sits beneath it")
	routed, ok := off.base.(providerTransport)
	require.True(t, ok)
	streams, ok := routed.base.(eventStreamTransport)
	require.True(t, ok)
	require.Equal(t, http.DefaultTransport, streams.base)

	p.Reasoning = ""
	off, ok = p.modelClient().Transport.(reasoningOffTransport)
	require.True(t, ok)
	routed, ok = off.base.(providerTransport)
	require.True(t, ok, "no effort still gets provider routing")
	_, ok = routed.base.(eventStreamTransport)
	require.True(t, ok, "and stream filtering")

	// The provider's own client keeps its bounded transport underneath.
	off, ok = OpenAIProvider{}.modelClient().Transport.(reasoningOffTransport)
	require.True(t, ok)
	routed, ok = off.base.(providerTransport)
	require.True(t, ok)
	own, ok := routed.base.(eventStreamTransport)
	require.True(t, ok)
	_, ok = own.base.(*http.Transport)
	require.True(t, ok)
}

func TestReasoningTransportPassesOtherRequestsThrough(t *testing.T) {
	var seen *http.Request
	transport := reasoningTransport{effort: ReasoningHigh, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	})}
	for _, req := range []*http.Request{
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/v1/models", nil),
		httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/chat/completions", strings.NewReader(`{}`)),
		httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", http.NoBody),
	} {
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Same(t, req, seen)
	}
}

func TestReasoningTransportRewritesTheBodyItSends(t *testing.T) {
	var sent []byte
	var replay []byte
	transport := reasoningTransport{effort: ReasoningMedium, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var err error
		sent, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, int64(len(sent)), r.ContentLength)
		again, err := r.GetBody()
		require.NoError(t, err)
		replay, err = io.ReadAll(again)
		require.NoError(t, err)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	})}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", strings.NewReader(`{"model":"m","input":"a && b < c"}`))
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.JSONEq(t, `{"model":"m","input":"a && b < c","reasoning":{"effort":"medium"}}`, string(sent))
	require.Contains(t, string(sent), "a && b < c", "prompt text must not be HTML-escaped")
	require.Equal(t, sent, replay, "a redirect replay must send the same rewritten body")
}

func TestReasoningTransportFailsARequestItCannotRewrite(t *testing.T) {
	called := false
	transport := reasoningTransport{effort: ReasoningHigh, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unreachable")
	})}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", strings.NewReader(`not json`))
	_, err := transport.RoundTrip(req) //nolint:bodyclose // the transport fails before any response exists
	require.ErrorContains(t, err, "set reasoning effort")
	require.False(t, called, "a request without the configured effort must not be sent")

	broken := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", io.MultiReader(strings.NewReader("{"), failingBody{}))
	_, err = transport.RoundTrip(broken) //nolint:bodyclose // the transport fails before any response exists
	require.ErrorContains(t, err, "read request body")
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }

func TestWithReasoningEffortMergesIntoExistingSettings(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"absent":   {in: `{"model":"m"}`, want: `{"model":"m","reasoning":{"effort":"low"}}`},
		"null":     {in: `{"model":"m","reasoning":null}`, want: `{"model":"m","reasoning":{"effort":"low"}}`},
		"summary":  {in: `{"reasoning":{"summary":"auto"}}`, want: `{"reasoning":{"summary":"auto","effort":"low"}}`},
		"override": {in: `{"reasoning":{"effort":"high"}}`, want: `{"reasoning":{"effort":"low"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withReasoningEffort([]byte(tc.in), ReasoningLow)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
	_, err := withReasoningEffort([]byte(`{"reasoning":"high"}`), ReasoningLow)
	require.Error(t, err)
}

func TestEventStreamTransportFiltersOnlySuccessfulStreams(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		contentType string
		filtered    bool
	}{
		"stream":            {status: http.StatusOK, contentType: "text/event-stream", filtered: true},
		"stream with param": {status: http.StatusOK, contentType: "Text/Event-Stream; charset=utf-8", filtered: true},
		"json":              {status: http.StatusOK, contentType: "application/json"},
		"missing":           {status: http.StatusOK},
		"error status":      {status: http.StatusTooManyRequests, contentType: "text/event-stream"},
	} {
		t.Run(name, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader(sseKeepalive))
			transport := eventStreamTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				header := http.Header{"Content-Length": {"24"}}
				if tc.contentType != "" {
					header.Set("Content-Type", tc.contentType)
				}
				return &http.Response{StatusCode: tc.status, Body: body, Header: header, ContentLength: 24}, nil
			})}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", nil)
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			_, filtered := resp.Body.(*eventStreamReader)
			require.Equal(t, tc.filtered, filtered)
			if filtered {
				require.Equal(t, int64(-1), resp.ContentLength)
				require.Empty(t, resp.Header.Get("Content-Length"))
			}
		})
	}
}

func TestEventStreamTransportPassesTransportErrorsThrough(t *testing.T) {
	refused := errors.New("connection refused")
	transport := eventStreamTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, refused })}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/v1/responses", nil)
	resp, err := transport.RoundTrip(req) //nolint:bodyclose // the base returned no response
	require.ErrorIs(t, err, refused)
	require.Nil(t, resp)
}
