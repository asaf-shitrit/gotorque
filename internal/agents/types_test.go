package agents

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDecodeFixtureShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ProposedFixture
		ok   bool
	}{
		{"path object", `{"path":"a.txt","content":"x"}`, ProposedFixture{Path: "a.txt", Content: "x"}, true},
		{"bare path string", `"a.txt"`, ProposedFixture{Path: "a.txt"}, true},
		{"name key with content", `{"name":"n.txt","content":"c"}`, ProposedFixture{Path: "n.txt", Content: "c"}, true},
		{"file key without content", `{"file":"f.txt"}`, ProposedFixture{Path: "f.txt"}, true},
		{"filename key, non-string content dropped", `{"filename":"g.txt","content":42}`, ProposedFixture{Path: "g.txt"}, true},
		{"id key", `{"id":"i.txt"}`, ProposedFixture{Path: "i.txt"}, true},
		{"name outranks id", `{"id":"i.txt","name":"n.txt"}`, ProposedFixture{Path: "n.txt"}, true},
		{"empty path falls through to name", `{"path":"","name":"n.txt"}`, ProposedFixture{Path: "n.txt"}, true},
		{"empty name skipped", `{"name":"","file":"f.txt"}`, ProposedFixture{Path: "f.txt"}, true},
		{"non-string identifier skipped", `{"name":7}`, ProposedFixture{}, false},
		{"no identifying key", `{"other":"x"}`, ProposedFixture{}, false},
		{"empty string", `""`, ProposedFixture{}, false},
		{"number", `42`, ProposedFixture{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeFixture(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%+v, %v) want (%+v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFlexDimsCoercion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want flexDims
	}{
		{"null left unset", `null`, nil},
		{"empty input left unset", ``, nil},
		{"array left unset", `[1,2]`, nil},
		{"string left unset", `"x"`, nil},
		{"number", `{"n":3}`, flexDims{"n": 3}},
		{"negative number", `{"n":-3}`, flexDims{"n": -3}},
		{"numeric string", `{"n":"4"}`, flexDims{"n": 4}},
		{"padded numeric string", `{"n":" 5 "}`, flexDims{"n": 5}},
		{"surrounding whitespace", `  {"n":1}  `, flexDims{"n": 1}},
		{"fraction dropped", `{"n":2.5}`, flexDims{}},
		{"exponent form dropped", `{"n":1e3}`, flexDims{}},
		{"non-numeric string dropped", `{"n":"abc"}`, flexDims{}},
		{"bool dropped", `{"n":true}`, flexDims{}},
		{"null value dropped", `{"n":null}`, flexDims{}},
		{"mixed keeps only usable", `{"a":1,"b":"2","c":"x","d":2.5}`, flexDims{"a": 1, "b": 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got flexDims
			if err := got.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}

	var got flexDims
	if err := got.UnmarshalJSON([]byte(`{bad`)); err == nil {
		t.Fatalf("malformed object: expected error, got %#v", got)
	}
}
