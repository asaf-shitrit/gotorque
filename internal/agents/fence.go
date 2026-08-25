package agents

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// RoleUsage aggregates token usage across every model call made for one role.
type RoleUsage struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// UsageCollector accumulates per-role token usage from decorated models.
// It is safe for concurrent use; the zero value is ready for Record.
type UsageCollector struct {
	mu     sync.Mutex
	ByRole map[string]*RoleUsage
}

// NewUsageCollector returns an empty collector keyed by role name.
func NewUsageCollector() *UsageCollector {
	return &UsageCollector{ByRole: map[string]*RoleUsage{}}
}

// Record folds one response's usage metadata into the running totals for
// role. Nil collectors and nil metadata are ignored so decoration stays
// transparent when an endpoint omits usage data.
func (c *UsageCollector) Record(role string, u *genai.GenerateContentResponseUsageMetadata) {
	if c == nil || u == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ByRole == nil {
		c.ByRole = map[string]*RoleUsage{}
	}
	usage := c.ByRole[role]
	if usage == nil {
		usage = &RoleUsage{}
		c.ByRole[role] = usage
	}
	usage.Requests++
	usage.PromptTokens += int64(u.PromptTokenCount)
	usage.CompletionTokens += int64(u.CandidatesTokenCount)
	usage.TotalTokens += int64(u.TotalTokenCount)
}

// Snapshot returns a copy of the per-role totals keyed by role name.
func (c *UsageCollector) Snapshot() map[string]RoleUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(map[string]RoleUsage, len(c.ByRole))
	for role, usage := range c.ByRole {
		if usage == nil {
			continue
		}
		snapshot[role] = *usage
	}
	return snapshot
}

// fenceStrippingModel wraps an LLM and removes Markdown code fences that
// models frequently wrap around JSON payloads. Typed workflow nodes parse
// model text as raw JSON, so an otherwise valid response like
// "```json\n{...}\n```" would fail output validation. It also records token
// usage metadata into a shared collector when one is supplied.
type fenceStrippingModel struct {
	inner model.LLM
	role  string
	usage *UsageCollector
}

// NewFenceStrippingModel decorates an LLM so fenced JSON responses are
// normalized before any downstream validation runs. The role labels the
// collector's per-role totals; the collector may be nil to skip tracking.
func NewFenceStrippingModel(inner model.LLM, role string, usage *UsageCollector) model.LLM {
	if inner == nil {
		return nil
	}
	return &fenceStrippingModel{inner: inner, role: role, usage: usage}
}

func (m fenceStrippingModel) Name() string { return m.inner.Name() }

func (m fenceStrippingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Transparent retries: shared-pool rate limits (HTTP 429),
		// transport errors, and complete-but-unparseable JSON payloads all
		// otherwise abort a multi-hour campaign on a single bad roll.
		const attempts = 4
		var backoff time.Duration
		yieldedAny := false
		for attempt := 0; attempt < attempts; attempt++ {
			if backoff > 0 {
				select {
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				case <-time.After(backoff):
				}
			}
			retrying := false
			for resp, err := range m.inner.GenerateContent(ctx, req, stream) {
				if err != nil {
					if !yieldedAny && attempt < attempts-1 {
						retrying = true
						break
					}
					if !yield(resp, err) {
						return
					}
					continue
				}
				rewriteResponse(resp)
				m.usage.Record(m.role, resp.UsageMetadata)
				if !resp.Partial && !responseTextIsJSON(resp) && !yieldedAny && attempt < attempts-1 {
					retrying = true
					break
				}
				yieldedAny = true
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

// responseTextIsJSON reports whether the complete response's visible text
// parses as JSON after fence/prose unwrapping.
func responseTextIsJSON(resp *model.LLMResponse) bool {
	if resp == nil || resp.Content == nil {
		return false
	}
	var b strings.Builder
	for _, part := range resp.Content.Parts {
		if part != nil && !part.Thought {
			b.WriteString(part.Text)
		}
	}
	var payload any
	return json.Unmarshal([]byte(UnwrapJSONFence(b.String())), &payload) == nil
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
