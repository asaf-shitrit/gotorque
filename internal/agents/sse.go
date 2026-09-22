package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrStreamIncomplete reports a model stream that ended without the endpoint
// saying the response was finished.
//
// ADK's openaimodel reads the Responses API stream through openai-go, and
// neither layer can tell a finished answer from a cut one. openai-go treats a
// clean end of the connection as the end of the stream, so a stream closed
// before response.completed (a dropped connection, a proxy reaping a socket)
// yields whatever text had arrived as the final answer. ADK also ignores
// response.incomplete, which the endpoint sends when the answer hits
// max_output_tokens. Either way the decoder received a JSON payload cut
// mid-object with finish reason Unspecified, and read it as a model that
// answered badly. As an error, the call is retried by the ladder in fence.go.
//
// The error carries no HTTP status on purpose: the ladder stops early on
// client status errors, and a cut stream is not one.
var ErrStreamIncomplete = errors.New("model stream ended without a completed response")

// eventStreamReader filters a text/event-stream body before openai-go parses
// it.
//
// openai-go's decoder dispatches an event on every blank line, including the
// one that ends a comment-only event such as OpenRouter's documented
// `: OPENROUTER PROCESSING` keepalive, then json.Unmarshals that event's empty
// data and fails the whole call with `unexpected end of JSON input`. The
// reader withholds an event's lines until a data line proves it is a real
// event, and discards the event whole if its blank line comes first. Events
// that carry data pass through byte for byte.
//
// It also watches the type of every event it passes and turns an end of
// stream that no terminal event preceded into ErrStreamIncomplete.
//
// The body is read only when the caller wants bytes the reader does not
// already hold, so it blocks exactly as long as the body does and unblocks
// when a cancelled request closes it. A discarded keepalive produces nothing
// upstream, so it never counts as activity against streamIdleTimeout.
type eventStreamReader struct {
	body  io.ReadCloser
	chunk []byte
	// in holds bytes read from body but not yet split into lines.
	in []byte
	// out holds filtered bytes the caller has not read yet.
	out bytes.Buffer
	// held holds the current event's lines while it has carried no data.
	held []byte
	// data and name are the current event's payload and event: field, kept
	// to classify it once it is dispatched.
	data     []byte
	name     string
	carries  bool
	terminal bool
	// err is returned once out is drained. It is sticky.
	err error
}

func newEventStreamReader(body io.ReadCloser) *eventStreamReader {
	return &eventStreamReader{body: body, chunk: make([]byte, 32<<10)}
}

func (r *eventStreamReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && r.err == nil {
		r.fill()
	}
	if r.out.Len() > 0 {
		return r.out.Read(p)
	}
	return 0, r.err
}

func (r *eventStreamReader) Close() error { return r.body.Close() }

// fill performs one read of the body and filters every complete line it
// finished.
func (r *eventStreamReader) fill() {
	n, err := r.body.Read(r.chunk)
	r.in = append(r.in, r.chunk[:n]...)
	rest := r.in
	for {
		end := bytes.IndexByte(rest, '\n')
		if end < 0 {
			break
		}
		r.line(rest[:end+1])
		rest = rest[end+1:]
	}
	r.in = append(r.in[:0], rest...)
	if err != nil {
		r.fail(r.finish(err))
	}
}

// line routes one newline-terminated line. Field parsing mirrors openai-go's
// decoder, including its tolerance of a CRLF line ending, so the reader and
// the SDK agree on which events carry data.
func (r *eventStreamReader) line(line []byte) {
	content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	if len(content) == 0 {
		r.dispatch(line)
		return
	}
	field, value, _ := bytes.Cut(content, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "data":
		r.data = append(append(r.data, value...), '\n')
		if !r.carries {
			r.carries = true
			r.out.Write(r.held)
			r.held = r.held[:0]
		}
	case "event":
		r.name = string(value)
	}
	r.emit(line)
}

func (r *eventStreamReader) emit(line []byte) {
	if r.carries {
		r.out.Write(line)
		return
	}
	r.held = append(r.held, line...)
}

// dispatch ends the current event at its blank line: an event that carried
// data is passed on and classified, and one that did not is dropped with its
// blank line, which is what would have made openai-go dispatch it.
func (r *eventStreamReader) dispatch(blank []byte) {
	if r.carries {
		r.out.Write(blank)
		r.classify()
	}
	r.held, r.data, r.name, r.carries = r.held[:0], r.data[:0], "", false
}

// classify records whether the event just passed ends the response.
//
// response.failed and error events already fail the call inside ADK, so they
// only need to count as terminal. response.incomplete is the one ADK drops on
// the floor; it fails the stream right after the event, naming the reason
// the endpoint gave, instead of waiting for the connection to close.
func (r *eventStreamReader) classify() {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	kind := r.name
	if json.Unmarshal(r.data, &event) == nil && event.Type != "" {
		kind = event.Type
	}
	switch kind {
	case "response.completed", "response.failed", "error":
		r.terminal = true
	case "response.incomplete":
		r.terminal = true
		r.fail(incompleteError(event.Response.IncompleteDetails.Reason))
	}
}

// finish maps the body's final error. Transport errors pass through
// untouched; only a clean end of stream is judged against what was seen.
func (r *eventStreamReader) finish(err error) error {
	if !errors.Is(err, io.EOF) || r.terminal {
		return err
	}
	return fmt.Errorf("%w: the connection closed before response.completed", ErrStreamIncomplete)
}

func (r *eventStreamReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func incompleteError(reason string) error {
	if reason == "" {
		reason = "not given"
	}
	return fmt.Errorf("%w: the endpoint sent response.incomplete (reason: %s)", ErrStreamIncomplete, reason)
}
