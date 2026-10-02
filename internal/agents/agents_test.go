package agents

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
)

type stubModel struct{ name string }

func (m stubModel) Name() string { return m.name }

func (stubModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

func TestNewSetBuildsTheOptimizerOnTheInjectedModel(t *testing.T) {
	calls := 0
	set, err := NewSet(context.Background(), ModelProviderFunc(func(context.Context) (model.LLM, error) {
		calls++
		return stubModel{name: "optimizer"}, nil
	}))
	require.NoError(t, err)
	require.Equal(t, 1, calls, "the optimizer is the only model role")
	require.NotNil(t, set.Optimizer)
	require.Equal(t, "optimizer", set.Optimizer.Name())
}

func TestNewSetReportsProviderFailure(t *testing.T) {
	wantErr := errors.New("model unavailable")
	_, err := NewSet(context.Background(), ModelProviderFunc(func(context.Context) (model.LLM, error) {
		return nil, wantErr
	}))
	require.ErrorIs(t, err, wantErr)
}

func TestNewSetRejectsMissingProviderAndNilModel(t *testing.T) {
	_, err := NewSet(context.Background(), nil)
	require.ErrorContains(t, err, "model provider is required")
	// A provider that answers with neither a model nor an error is exactly
	// the case NewSet must refuse.
	_, err = NewSet(context.Background(), ModelProviderFunc(func(context.Context) (model.LLM, error) { return nil, nil })) //nolint:nilnil // the nil model is the case under test
	require.ErrorContains(t, err, "optimizer model is nil")
}

// A target's kind rides in its JSON, and a target saved before kinds existed
// (no "kind" key) reads back as the one-function kind.
func TestTargetKindRoundTripsAndDefaultsToOneFunction(t *testing.T) {
	set := Target{Location: "a.go:9", Function: "(*Store).Get", Cause: "throwaway_result", Kind: TargetFunctionSet,
		Callers: []FunctionRef{{Name: "(*Store).IsPositive", Location: "b.go:3"}}}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var back Target
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.IsFunctionSet() || !reflect.DeepEqual(back, set) {
		t.Fatalf("round trip = %#v, want %#v", back, set)
	}

	var old Target
	if err := json.Unmarshal([]byte(`{"location":"a.go:9","function":"f","cause":"alloc","remedy":"r","z":1}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.IsFunctionSet() || old.Kind != TargetFunction || old.Callers != nil {
		t.Fatalf("a target without a kind must read as one function: %#v", old)
	}
}
