package agents

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/model"
)

type stubModel struct{ name string }

func (m stubModel) Name() string { return m.name }

func (stubModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

func TestNewSetUsesInjectedModelsForEveryRole(t *testing.T) {
	var got []Role
	set, err := NewSet(context.Background(), ModelProviderFunc(func(_ context.Context, role Role) (model.LLM, error) {
		got = append(got, role)
		return stubModel{name: string(role)}, nil
	}))
	if err != nil {
		t.Fatalf("NewSet() error = %v", err)
	}
	if !reflect.DeepEqual(got, AllRoles) {
		t.Fatalf("provider roles = %v, want %v", got, AllRoles)
	}

	wantNames := []string{"coordinator", "explorer", "analyst", "optimizer", "reviewer"}
	for i, a := range set.All() {
		if a == nil {
			t.Fatalf("agent %d is nil", i)
		}
		if a.Name() != wantNames[i] {
			t.Errorf("agent %d name = %q, want %q", i, a.Name(), wantNames[i])
		}
	}
}

func TestNewSetReportsProviderFailure(t *testing.T) {
	wantErr := errors.New("model unavailable")
	_, err := NewSet(context.Background(), ModelProviderFunc(func(_ context.Context, role Role) (model.LLM, error) {
		if role == RoleAnalyst {
			return nil, wantErr
		}
		return stubModel{name: string(role)}, nil
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewSet() error = %v, want wrapped %v", err, wantErr)
	}
}

func TestRoleResponseSchemaCoversEveryRole(t *testing.T) {
	for _, role := range AllRoles {
		schema, err := roleResponseSchema(role)
		if err != nil {
			t.Fatalf("roleResponseSchema(%s) error = %v", role, err)
		}
		if schema["type"] != "object" {
			t.Errorf("roleResponseSchema(%s) type = %v, want object", role, schema["type"])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Errorf("roleResponseSchema(%s) has no properties", role)
		}
	}
}

func TestRoleResponseSchemaRejectsUnknownRole(t *testing.T) {
	if _, err := roleResponseSchema(Role("nonexistent")); err == nil {
		t.Fatal("roleResponseSchema(nonexistent) error = nil, want registration failure")
	}
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
