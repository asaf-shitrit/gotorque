package agents

import (
	"context"
	"iter"
	"time"
	"strings"

	"google.golang.org/adk/v2/model"
)

// fenceStrippingModel wraps an LLM and removes Markdown code fences that
// models frequently wrap around JSON payloads. Typed workflow nodes parse
// model text as raw JSON, so an otherwise valid response like
// "```json\n{...}\n```" would fail output validation.
type fenceStrippingModel struct {
	inner model.LLM
}

// NewFenceStrippingModel decorates an LLM so fenced JSON responses are
// normalized before any downstream validation runs.
func NewFenceStrippingModel(inner model.LLM) model.LLM {
	if inner == nil {
		return nil
	}
	return fenceStrippingModel{inner: inner}
}

func (m fenceStrippingModel) Name() string { return m.inner.Name() }

func (m fenceStrippingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Transparent retries while a call fails before producing any
		// content: shared-pool rate limits (HTTP 429) and transient
		// endpoint errors otherwise abort a whole multi-hour campaign.
		const attempts = 4
		var backoff time.Duration
		for attempt := 0; attempt < attempts; attempt++ {
			if backoff > 0 {
				select {
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				case <-time.After(backoff):
				}
			}
			producedAny := false
			retrying := false
			for resp, err := range m.inner.GenerateContent(ctx, req, stream) {
				if err != nil {
					if !producedAny && attempt < attempts-1 {
						retrying = true
						break
					}
					if !yield(resp, err) {
						return
					}
					continue
				}
				producedAny = true
				rewriteResponse(resp)
				if !yield(resp, nil) {
					return
				}
			}
			if !retrying {
				return
			}
			backoff = time.Duration(1<<uint(attempt)) * 15 * time.Second // 15s, 30s, 60s
		}
	}
}

func rewriteResponse(resp *model.LLMResponse) {
	// Only rewrite complete responses; streaming partials may split a
	// fence across chunks and must pass through untouched.
	if resp.Content == nil || resp.Partial {
		return
	}
	for _, part := range resp.Content.Parts {
		if part != nil && part.Text != "" {
			part.Text = UnwrapJSONFence(part.Text)
		}
	}
}

// UnwrapJSONFence strips a single Markdown code fence (with optional
// language tag such as "json") surrounding a JSON object or array, and
// extracts a balanced JSON payload from surrounding prose. Text without
// any extractable JSON payload is returned unchanged.
func UnwrapJSONFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if start := strings.Index(trimmed, "```"); start >= 0 {
		body := trimmed[start+3:]
		newline := strings.IndexByte(body, '\n')
		if newline < 0 {
			return text
		}
		body = body[newline+1:]
		if end := strings.LastIndex(body, "```"); end >= 0 {
			body = body[:end]
		}
		trimmed = strings.TrimSpace(body)
	}
	if payload, ok := balancedJSON(trimmed); ok {
		return payload
	}
	return text
}

// balancedJSON returns the first balanced JSON object or array in s,
// ignoring braces inside JSON strings. It returns false when s contains
// no complete JSON container.
func balancedJSON(s string) (string, bool) {
	start := -1
	var open byte
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			if depth > 0 {
				inString = true
			}
		case '{', '[':
			if depth == 0 {
				start = i
				open = c
			}
			depth++
		case '}', ']':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				close := byte('}')
				if open == '[' {
					close = ']'
				}
				if c != close {
					return "", false
				}
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
