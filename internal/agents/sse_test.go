package agents

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
)

// Responses API events in the shape OpenAI and OpenRouter send them: an
// event: line naming the type, then one data: line carrying it again.
const (
	sseCreated    = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n"
	sseItemAdded  = "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n"
	sseDeltaOne   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"{\\\"ok\\\":\"}\n\n"
	sseDeltaTwo   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"true}\"}\n\n"
	sseCompleted  = "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"m\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"ok\\\":true}\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"
	sseIncomplete = "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"incomplete\",\"model\":\"m\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[]}}\n\n"
	sseFailed     = "event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"failed\",\"model\":\"m\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"},\"output\":[]}}\n\n"
	sseError      = "event: error\ndata: {\"type\":\"error\",\"sequence_number\":4,\"code\":\"server_error\",\"message\":\"boom\",\"param\":null}\n\n"
	sseKeepalive  = ": OPENROUTER PROCESSING\n\n"
)

func readFiltered(t *testing.T, body io.Reader) (string, error) {
	t.Helper()
	out, err := io.ReadAll(newEventStreamReader(io.NopCloser(body)))
	return string(out), err
}

func TestEventStreamReaderDropsKeepalivesAndPassesEventsVerbatim(t *testing.T) {
	events := sseCreated + sseItemAdded + sseDeltaOne + sseDeltaTwo + sseCompleted
	stream := sseKeepalive + sseCreated + sseKeepalive + sseItemAdded + sseDeltaOne + sseKeepalive + sseKeepalive + sseDeltaTwo + sseCompleted
	for name, body := range map[string]io.Reader{
		"whole":          strings.NewReader(stream),
		"byte at a time": iotest.OneByteReader(strings.NewReader(stream)),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readFiltered(t, body)
			require.NoError(t, err)
			require.Equal(t, events, got)
		})
	}
}

// Every field shape openai-go dispatches on a blank line, whether or not it
// carries data. Only the ones that carry data may survive.
func TestEventStreamReaderDropsEveryEventWithoutData(t *testing.T) {
	stream := ": comment\r\n\r\n" +
		"event: response.in_progress\n\n" +
		"id: 7\nretry: 100\n\n" +
		"\n" +
		sseCompleted
	got, err := readFiltered(t, strings.NewReader(stream))
	require.NoError(t, err)
	require.Equal(t, sseCompleted, got)
}

// A comment or id inside an event that carries data is part of that event,
// so it passes through with it, in place.
func TestEventStreamReaderKeepsLinesOfAnEventWithData(t *testing.T) {
	event := ": inline\r\nid: 9\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\n: between\r\ndata: \"response\":{}}\r\n\r\n"
	got, err := readFiltered(t, strings.NewReader(sseKeepalive+event))
	require.NoError(t, err)
	require.Equal(t, event, got)
}

func TestEventStreamReaderFailsAStreamCutBeforeCompletion(t *testing.T) {
	got, err := readFiltered(t, strings.NewReader(sseCreated+sseDeltaOne+sseKeepalive))
	require.ErrorIs(t, err, ErrStreamIncomplete)
	require.ErrorContains(t, err, "before response.completed")
	// The events that did arrive are still delivered, so ADK processes them
	// before it sees the error.
	require.Equal(t, sseCreated+sseDeltaOne, got)
}

func TestEventStreamReaderFailsAnEmptyStream(t *testing.T) {
	_, err := readFiltered(t, strings.NewReader(sseKeepalive))
	require.ErrorIs(t, err, ErrStreamIncomplete)
}

// A completed event whose blank line never arrived is not dispatched by
// openai-go either, so the stream did not complete.
func TestEventStreamReaderFailsAnUndispatchedCompletion(t *testing.T) {
	_, err := readFiltered(t, strings.NewReader(sseCreated+strings.TrimSuffix(sseCompleted, "\n")))
	require.ErrorIs(t, err, ErrStreamIncomplete)
}

func TestEventStreamReaderNamesTheIncompleteReason(t *testing.T) {
	got, err := readFiltered(t, strings.NewReader(sseCreated+sseDeltaOne+sseIncomplete))
	require.ErrorIs(t, err, ErrStreamIncomplete)
	require.ErrorContains(t, err, "response.incomplete (reason: max_output_tokens)")
	require.Equal(t, sseCreated+sseDeltaOne+sseIncomplete, got)
}

func TestEventStreamReaderReportsAnIncompleteWithoutReason(t *testing.T) {
	// Some OpenAI-compatible endpoints send the event with only a data line.
	event := "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":null}}\n\n"
	_, err := readFiltered(t, strings.NewReader(event))
	require.ErrorIs(t, err, ErrStreamIncomplete)
	require.ErrorContains(t, err, "reason: not given")
}

// response.incomplete ends the response, so the error must not wait for the
// endpoint to close the connection.
func TestEventStreamReaderFailsAtTheIncompleteEventNotAtClose(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	go func() { _, _ = pw.Write([]byte(sseCreated + sseIncomplete)) }()
	reader := newEventStreamReader(pr)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "max_output_tokens")
	case <-time.After(5 * time.Second):
		t.Fatal("reader waited for the connection to close after response.incomplete")
	}
}

// response.failed and error events are already fatal inside ADK, which then
// closes the stream; they count as terminal so the reader adds nothing.
func TestEventStreamReaderPassesFailureEventsThrough(t *testing.T) {
	for name, event := range map[string]string{"failed": sseFailed, "error": sseError} {
		t.Run(name, func(t *testing.T) {
			got, err := readFiltered(t, strings.NewReader(sseCreated+event))
			require.NoError(t, err)
			require.Equal(t, sseCreated+event, got)
		})
	}
}

// The event: field classifies an event whose data is not a typed object.
func TestEventStreamReaderFallsBackToTheEventField(t *testing.T) {
	event := "event: response.completed\ndata: [DONE]\n\n"
	got, err := readFiltered(t, strings.NewReader(event))
	require.NoError(t, err)
	require.Equal(t, event, got)
}

func TestEventStreamReaderPassesTransportErrorsThrough(t *testing.T) {
	cut := errors.New("connection reset")
	body := io.MultiReader(strings.NewReader(sseCreated), iotest.ErrReader(cut))
	got, err := readFiltered(t, body)
	require.ErrorIs(t, err, cut)
	require.NotErrorIs(t, err, ErrStreamIncomplete)
	require.Equal(t, sseCreated, got)
}

// A keepalive must not surface as a read: it is not activity, and the idle
// bound in stream.go has to see the silence it stands for. When the request
// is cancelled the body fails, and the reader must return at once.
func TestEventStreamReaderBlocksThroughKeepalivesUntilTheBodyFails(t *testing.T) {
	pr, pw := io.Pipe()
	reader := newEventStreamReader(pr)
	reads := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := reader.Read(buf)
		reads <- err
	}()
	for range 3 {
		_, err := pw.Write([]byte(sseKeepalive))
		require.NoError(t, err)
	}
	select {
	case err := <-reads:
		t.Fatalf("Read returned (err=%v) while only keepalives arrived", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelled := errors.New("request cancelled")
	pw.CloseWithError(cancelled)
	select {
	case err := <-reads:
		require.ErrorIs(t, err, cancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("Read kept blocking after the body failed")
	}
}

func TestEventStreamReaderCloseClosesTheBody(t *testing.T) {
	pr, pw := io.Pipe()
	require.NoError(t, newEventStreamReader(pr).Close())
	_, err := pw.Write([]byte("x"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
}
