package agents

import (
	"context"
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
