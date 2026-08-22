package agents

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestUnwrapJSONFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain json unchanged", `{"a":1}`, `{"a":1}`},
		{"fenced json", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced bare", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced array", "```json\n[1,2]\n```", `[1,2]`},
		{"prose before fence is dropped when json follows", "Here you go:\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{"non-json fenced code preserved", "```go\nfmt.Println()\n```", "```go\nfmt.Println()\n```"},
		{"unclosed fence still unwrapped when json body", "```json\n{\"a\":1}", `{"a":1}`},
		{"nested braces survive", "```json\n{\"patch\":\"diff with ``` inside\"}\n```", `{"patch":"diff with ` + "```" + ` inside"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnwrapJSONFence(tc.input); got != tc.want {
				t.Fatalf("UnwrapJSONFence() = %q, want %q", got, tc.want)
			}
		})
	}
}

type fakeLLM struct {
	resp *model.LLMResponse
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(f.resp, nil)
	}
}

func TestFenceStrippingModelRewritesCompleteResponses(t *testing.T) {
	inner := &fakeLLM{resp: &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "```json\n{\"objective\":\"x\"}\n```"}}}}}
	decorated := NewFenceStrippingModel(inner)
	if decorated == nil {
		t.Fatal("NewFenceStrippingModel returned nil")
	}
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := resp.Content.Parts[0].Text
		if got != `{"objective":"x"}` {
			t.Fatalf("text = %q, want stripped JSON", got)
		}
	}
}

func TestFenceStrippingModelLeavesPartialsUntouched(t *testing.T) {
	inner := &fakeLLM{resp: &model.LLMResponse{Partial: true, Content: &genai.Content{Parts: []*genai.Part{{Text: "```json\n{\"obj"}}}}}
	decorated := NewFenceStrippingModel(inner).(fenceStrippingModel)
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Content.Parts[0].Text != "```json\n{\"obj" {
			t.Fatalf("partial was rewritten: %q", resp.Content.Parts[0].Text)
		}
	}
}

func TestNewFenceStrippingModelNil(t *testing.T) {
	if NewFenceStrippingModel(nil) != nil {
		t.Fatal("expected nil for nil inner model")
	}
}

func TestUnwrapJSONFenceProseExtraction(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"prose then json", "Here is my plan:\n{\"a\":1}", `{"a":1}`},
		{"json then trailing prose", "{\"a\":1}\nHope this helps!", `{"a":1}`},
		{"braces inside strings respected", `Prefix {"a":"}{"} suffix`, `{"a":"}{"}`},
		{"nested containers", `Answer: {"o":{"x":[1,2]},"y":"}"}`, `{"o":{"x":[1,2]},"y":"}"}`},
		{"array payload", "text before [1,{\"b\":2}] text after", `[1,{"b":2}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnwrapJSONFence(tc.input); got != tc.want {
				t.Fatalf("UnwrapJSONFence() = %q, want %q", got, tc.want)
			}
		})
	}
}

type flakyLLM struct {
	calls int
	fail  bool
	resp  *model.LLMResponse
}

func (f *flakyLLM) Name() string { return "flaky" }

func (f *flakyLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.calls++
		if f.fail && f.calls == 1 {
			yield(nil, context.DeadlineExceeded)
			return
		}
		yield(f.resp, nil)
	}
}

func TestFenceStrippingModelRetriesFirstFailure(t *testing.T) {
	inner := &flakyLLM{fail: true, resp: &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: `{"ok":1}`}}}}}
	decorated := NewFenceStrippingModel(inner)
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("expected retry to succeed, got %v", err)
		}
		if resp.Content.Parts[0].Text != `{"ok":1}` {
			t.Fatalf("unexpected text %q", resp.Content.Parts[0].Text)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one failure + one retry)", inner.calls)
	}
}
