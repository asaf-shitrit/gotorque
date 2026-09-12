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

func TestDecodeHotPathShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want HotPath
		ok   bool
	}{
		{"location object keeps all fields", `{"location":"a.go:12","impact":3,"evidence":"cpu","confidence":0.5}`, HotPath{Location: "a.go:12", Impact: 3, Evidence: "cpu", Confidence: 0.5}, true},
		{"bare location string", `"b.go:7"`, HotPath{Location: "b.go:7"}, true},
		{"name key", `{"name":"c.go:9"}`, HotPath{Location: "c.go:9"}, true},
		{"symbol outranks path", `{"path":"p.go:1","symbol":"S"}`, HotPath{Location: "S"}, true},
		{"object without identifier", `{"impact":3}`, HotPath{}, false},
		{"empty object", `{}`, HotPath{}, false},
		{"empty string", `""`, HotPath{}, false},
		{"number", `42`, HotPath{}, false},
		{"malformed json", `{bad`, HotPath{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeHotPath(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%+v, %v) want (%+v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFlexHotPathsShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want flexHotPaths
	}{
		{"null left unset", `null`, nil},
		{"empty input left unset", ``, nil},
		{"empty object left unset", `{}`, nil},
		{"array of location strings", `["a.go:1","b.go:2"]`, flexHotPaths{{Location: "a.go:1"}, {Location: "b.go:2"}}},
		{"array drops non-identifying elements", `["a.go:1",{"location":"b.go:2"},7,{}]`, flexHotPaths{{Location: "a.go:1"}, {Location: "b.go:2"}}},
		{"single hot path object", `{"location":"c.go:3","impact":2}`, flexHotPaths{{Location: "c.go:3", Impact: 2}}},
		{"single object via identifying key", `{"name":"d.go:4"}`, flexHotPaths{{Location: "d.go:4"}}},
		{"grouped measured and suspected", `{"measured":["m.go:1"],"suspected":[{"location":"s.go:2"}]}`, flexHotPaths{{Location: "m.go:1"}, {Location: "s.go:2"}}},
		{"grouped alias keys", `{"paths":["p.go:1"],"items":["i.go:2"]}`, flexHotPaths{{Location: "p.go:1"}, {Location: "i.go:2"}}},
		{"non-array group skipped", `{"suspected":"oops"}`, nil},
		{"non-array group does not block later group", `{"measured":5,"suspected":["s.go:2"]}`, flexHotPaths{{Location: "s.go:2"}}},
		{"unknown keys ignored", `{"other":["x"]}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got flexHotPaths
			if err := got.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}

	var got flexHotPaths
	if err := got.UnmarshalJSON([]byte(`{bad`)); err == nil {
		t.Fatalf("malformed object: expected error, got %#v", got)
	}
}
